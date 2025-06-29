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

	// Parse the form data
	const maxMemory = 10 << 20
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse form", err)
		return
	}

	// Get the image data from the form
	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get file from form", err)
		return
	}
	defer file.Close()

	// Get media type from Content-Type header using mime.ParseMediaType
	contentType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type header", err)
		return
	}

	// Validate that media type is either image/jpeg or image/png
	var fileExtension string
	switch mediaType {
	case "image/jpeg":
		fileExtension = "jpg"
	case "image/png":
		fileExtension = "png"
	default:
		respondWithError(w, http.StatusBadRequest, "Invalid file type. Only JPEG and PNG images are allowed", nil)
		return
	}

	// Get video metadata from database
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}

	// Check if authenticated user is the video owner
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "User not authorized to update this video", nil)
		return
	}

	// Generate random filename using crypto/rand
	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random filename", err)
		return
	}
	randomString := base64.RawURLEncoding.EncodeToString(randomBytes)

	// Create file path: /assets/<randomString>.<file_extension>
	filename := fmt.Sprintf("%s.%s", randomString, fileExtension)
	filePath := filepath.Join(cfg.assetsRoot, filename)

	// Create the new file
	newFile, err := os.Create(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create file", err)
		return
	}
	defer newFile.Close()

	// Copy contents from multipart.File to the new file
	_, err = io.Copy(newFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't write file", err)
		return
	}

	// Update video metadata with thumbnail URL
	thumbnailURL := fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, filename)
	video.ThumbnailURL = &thumbnailURL

	// Update the record in database
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	// Respond with updated video metadata
	respondWithJSON(w, http.StatusOK, video)
}
