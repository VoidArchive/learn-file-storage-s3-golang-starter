package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

type FFProbeOutput struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	// Create a presign client
	presignClient := s3.NewPresignClient(s3Client)

	// Create presigned URL
	presignedReq, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to create presigned URL: %w", err)
	}

	return presignedReq.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	// Check if VideoURL exists and contains bucket,key format
	if video.VideoURL == nil || *video.VideoURL == "" {
		return video, nil // Return as-is if no VideoURL
	}

	// Split the VideoURL on comma to get bucket and key
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) != 2 {
		return video, fmt.Errorf("invalid video URL format, expected 'bucket,key' but got: %s", *video.VideoURL)
	}

	bucket := parts[0]
	key := parts[1]

	// Generate presigned URL (expires in 1 hour)
	presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, time.Hour)
	if err != nil {
		return video, fmt.Errorf("failed to generate presigned URL: %w", err)
	}

	// Update the video with presigned URL
	video.VideoURL = &presignedURL
	return video, nil
}

func processVideoForFastStart(filePath string) (string, error) {
	// Create output file path by appending .processing
	outputPath := filePath + ".processing"

	// Create the ffmpeg command
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputPath)

	// Run the command
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("failed to process video with ffmpeg: %w", err)
	}

	return outputPath, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	// Create the ffprobe command
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	// Create a buffer to capture stdout
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	// Run the command
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("failed to run ffprobe: %w", err)
	}

	// Parse the JSON output
	var output FFProbeOutput
	err = json.Unmarshal(stdout.Bytes(), &output)
	if err != nil {
		return "", fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	// Check if we have stream data
	if len(output.Streams) == 0 {
		return "", fmt.Errorf("no streams found in video")
	}

	// Get width and height from first stream
	width := output.Streams[0].Width
	height := output.Streams[0].Height

	if width == 0 || height == 0 {
		return "", fmt.Errorf("invalid dimensions: width=%d, height=%d", width, height)
	}

	// Calculate aspect ratio and determine category
	ratio := float64(width) / float64(height)

	// 16:9 = 1.777..., 9:16 = 0.5625
	// Using tolerance for rounding errors
	if ratio >= 1.7 && ratio <= 1.8 {
		return "16:9", nil
	} else if ratio >= 0.55 && ratio <= 0.58 {
		return "9:16", nil
	} else {
		return "other", nil
	}
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Set upload limit to 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)

	// Extract videoID from URL path parameters
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}

	// Authenticate the user
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

	// Parse the form data
	err = r.ParseMultipartForm(1 << 30) // 1 GB limit
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse form", err)
		return
	}

	// Get the video file from the form
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video file from form", err)
		return
	}
	defer file.Close()

	// Validate that it's an MP4 video
	contentType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type header", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type. Only MP4 videos are allowed", nil)
		return
	}

	// Create a temporary file
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temporary file", err)
		return
	}
	defer tempFile.Close()
	defer os.Remove(tempFile.Name())

	// Copy contents from multipart file to temp file
	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't write to temporary file", err)
		return
	}

	// Get the aspect ratio of the video
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't determine video aspect ratio", err)
		return
	}

	// Process video for fast start
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for fast start", err)
		return
	}
	defer os.Remove(processedFilePath) // Clean up processed file

	// Open the processed file for upload
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed file", err)
		return
	}
	defer processedFile.Close()

	// Determine prefix based on aspect ratio
	var prefix string
	switch aspectRatio {
	case "16:9":
		prefix = "landscape"
	case "9:16":
		prefix = "portrait"
	default:
		prefix = "other"
	}

	// Reset file pointer to beginning
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't reset file pointer", err)
		return
	}

	// Generate random key for S3 with prefix
	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random key", err)
		return
	}
	randomString := base64.RawURLEncoding.EncodeToString(randomBytes)
	s3Key := fmt.Sprintf("%s/%s.mp4", prefix, randomString)

	// Upload to S3 using the processed file
	_, err = cfg.s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3Key,
		Body:        processedFile,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload to S3", err)
		return
	}

	// Update video URL in database with bucket,key format
	videoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, s3Key)
	video.VideoURL = &videoURL

	// Update the record in database
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	// Convert to signed video before responding
	signedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate presigned URL", err)
		return
	}

	// Respond with updated video metadata
	respondWithJSON(w, http.StatusOK, signedVideo)
}
