package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
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

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory int64 = 10 << 20
	r.ParseMultipartForm(maxMemory)
	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mType := header.Header.Get("Content-Type")
	mimeType, _, err := mime.ParseMediaType(mType)
	if err != nil || mimeType != "image/jpeg" && mimeType != "image/png" {
		respondWithError(w, http.StatusInternalServerError, "error parsing media type", err)
		return
	}
	extensions, err := mime.ExtensionsByType(mimeType)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to find extension based on file type", err)
		return
	}
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "error fetching video metadata", err)
		return
	}

	key := make([]byte, 32)
	rand.Read(key)
	base64String := base64.RawURLEncoding.EncodeToString(key)

	fileStorageLocation := filepath.Join(cfg.assetsRoot, base64String)
	fileStorageLocation = fileStorageLocation + extensions[0]
	thumbnailFile, err := os.Create(fileStorageLocation)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to create file at location", err)
		return
	}
	_, err = io.Copy(thumbnailFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error writing thumbnail to file", err)
		return
	}

	root := fmt.Sprintf("http://localhost:%s/", cfg.port)
	thumbnailUrl := root + fileStorageLocation
	video.ThumbnailURL = &thumbnailUrl
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "error updating video thumbnail", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
