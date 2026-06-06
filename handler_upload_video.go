package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory int64 = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "error fetching video metadata", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "error fetching video metadata", err)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	r.ParseMultipartForm(maxMemory)
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()
	mType := header.Header.Get("Content-Type")
	mimeType, _, err := mime.ParseMediaType(mType)
	if err != nil || mimeType != "video/mp4" {
		respondWithError(w, http.StatusInternalServerError, "error parsing media type", err)
		return
	}
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error creating temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()
	io.Copy(tempFile, file)
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	aspectRatioPrefix := ""
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error getting video aspect ratio", err)
		return
	}
	switch aspectRatio {
	case "16:9":
		aspectRatioPrefix = "landscape/"
	case "9:16":
		aspectRatioPrefix = "portrait/"
	default:
		aspectRatioPrefix = "other/"
	}
	processedVideo, err := processVideoForFastStart(tempFile.Name())
	fastStartFile, err := os.Open(processedVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error opening fast start file", err)
		return
	}
	tempFile.Seek(0, io.SeekStart)
	key := make([]byte, 32)
	rand.Read(key)
	base64String := base64.RawURLEncoding.EncodeToString(key)
	keyString := aspectRatioPrefix + base64String + ".mp4"
	cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{Bucket: &cfg.s3Bucket, Key: &keyString, Body: fastStartFile, ContentType: &mimeType})
	//vidUrl := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, keyString)
	bucketKey := cfg.s3Bucket + "," + keyString
	video.VideoURL = &bucketKey
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "error updating video url", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't get presigned url", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func processVideoForFastStart(filePath string) (string, error) {
	outputString := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-codec", "copy", "-movflags", "faststart", "-f", "mp4", outputString)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg error: %s, %v", stderr.String(), err)
	}
	return outputString, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	preSignedClient := s3.NewPresignClient(s3Client)
	req, err := preSignedClient.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, s3.WithPresignExpires(expireTime))

	if err != nil {
		return "", fmt.Errorf("failed to sign request: %s ", err)
	}

	return req.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	var bucketKey []string
	if video.VideoURL != nil {
		oldUrl := video.VideoURL
		bucketKey = strings.Split(*oldUrl, ",")
	}
	if len(bucketKey) < 2 {
		return video, nil
	}
	presignedUrl, err := generatePresignedURL(cfg.s3Client, bucketKey[0], bucketKey[1], (15 * time.Minute))

	if err != nil {
		return video, fmt.Errorf("error generating presigen url: %s ", err)
	}
	video.VideoURL = &presignedUrl

	return video, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	type Stream struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	type FFprobeOutput struct {
		Streams []Stream `json:"streams"`
	}

	aspectRatioCommand := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var output bytes.Buffer
	aspectRatioCommand.Stdout = &output
	err := aspectRatioCommand.Run()
	if err != nil {
		return "", err
	}

	data := FFprobeOutput{}
	err = json.Unmarshal(output.Bytes(), &data)

	if err != nil {
		return "", err
	}

	ratio := float64(data.Streams[0].Width) / float64(data.Streams[0].Height)
	portrait := 9.0 / 16.0
	landscape := 16.0 / 9.0

	if math.Abs(ratio-landscape) < 0.05 {
		return "16:9", nil
	} else if math.Abs(ratio-portrait) < 0.05 {
		return "9:16", nil
	} else {
		return "other", nil
	}

}
