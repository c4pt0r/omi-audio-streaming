package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	agfs "github.com/c4pt0r/agfs/agfs-sdk/go"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	numChannels   = 1 // Mono audio
	sampleRate    = 16000
	bitsPerSample = 16 // 16 bits per sample
)

var (
	agfsClient *agfs.Client
	agfsUploadPath string
)

// CreateWAVHeader generates a WAV header for the given data length
func createWAVHeader(dataLength int) []byte {
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	header := make([]byte, 44)

	copy(header[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataLength))
	copy(header[8:12], []byte("WAVE"))

	copy(header[12:16], []byte("fmt "))
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], uint16(numChannels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:36], bitsPerSample)

	copy(header[36:40], []byte("data"))
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataLength))

	return header
}

func saveFileLocally(storageDir string, fileName string, tempFilePath string) error {
	// Create storage directory if it doesn't exist
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		return fmt.Errorf("failed to create storage directory: %v", err)
	}

	// Define destination path
	destPath := filepath.Join(storageDir, fileName)

	// Copy file from temp location to storage directory
	srcFile, err := os.Open(tempFilePath)
	if err != nil {
		return fmt.Errorf("failed to open source file: %v", err)
	}
	defer srcFile.Close()

	destFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %v", err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, srcFile); err != nil {
		return fmt.Errorf("failed to copy file: %v", err)
	}

	log.Printf("File %s saved to local storage directory %s successfully.", fileName, storageDir)
	return nil
}

func uploadToAGFS(filePath string, fileName string) error {
	if agfsClient == nil {
		return fmt.Errorf("AGFS client not initialized")
	}

	// Read file content
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %v", err)
	}

	// Construct full path with upload path prefix
	fullPath := fileName
	if agfsUploadPath != "" {
		fullPath = filepath.Join(agfsUploadPath, fileName)
	}

	// Upload to AGFS
	_, err = agfsClient.Write(fullPath, fileData)
	if err != nil {
		return fmt.Errorf("failed to upload to AGFS: %v", err)
	}

	log.Printf("File uploaded to AGFS at path: %s", fullPath)
	return nil
}

func handlePostAudio(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	sampleRateParam := query.Get("sample_rate")
	uid := query.Get("uid")

	log.Printf("Received request from uid: %s", uid)
	log.Printf("Requested sample rate: %s", sampleRateParam)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	currentTime := time.Now()
	filename := fmt.Sprintf("%02d_%02d_%04d_%02d_%02d_%02d.wav",
		currentTime.Day(),
		currentTime.Month(),
		currentTime.Year(),
		currentTime.Hour(),
		currentTime.Minute(),
		currentTime.Second())

	tempFilePath := filepath.Join(os.TempDir(), filename)

	header := createWAVHeader(len(body))

	// Write to temporary file
	tempFile, err := os.Create(tempFilePath)
	if err != nil {
		log.Printf("Failed to create temp file: %v", err)
		http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
		return
	}
	defer tempFile.Close()

	// Write WAV header and audio data
	tempFile.Write(header)
	tempFile.Write(body)

	// Upload to AGFS if client is configured
	if agfsClient != nil {
		err = uploadToAGFS(tempFilePath, filename)
		if err != nil {
			log.Printf("Failed to upload to AGFS: %v", err)
			// Fall back to local storage if AGFS upload fails
			storageDir := os.Getenv("AUDIO_STORAGE_DIR")
			if storageDir == "" {
				storageDir = "./audio_files"
			}
			err = saveFileLocally(storageDir, filename, tempFilePath)
			if err != nil {
				log.Printf("Failed to save file locally: %v", err)
				http.Error(w, "Failed to save file", http.StatusInternalServerError)
				return
			}
		} else {
			log.Printf("File uploaded to AGFS successfully, skipping local storage")
		}
	} else {
		// No AGFS configured, save to local storage
		storageDir := os.Getenv("AUDIO_STORAGE_DIR")
		if storageDir == "" {
			storageDir = "./audio_files"
		}
		err = saveFileLocally(storageDir, filename, tempFilePath)
		if err != nil {
			log.Printf("Failed to save file locally: %v", err)
			http.Error(w, "Failed to save file to local storage", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("Audio bytes received and saved as %s", filename)))
}

func main() {
	// Define command line flags
	addr := flag.String("addr", "", "Server address (default: :8080)")
	agfsAPIURL := flag.String("agfs-api-url", "", "AGFS client API URL")
	agfsPath := flag.String("agfs-upload-path", "", "AGFS upload path (e.g., /s3fs/aws/dongxu/omi-recording/)")
	flag.Parse()

	// Get address from environment variable or command line flag
	serverAddr := os.Getenv("SERVER_ADDR")
	if *addr != "" {
		serverAddr = *addr
	}
	if serverAddr == "" {
		serverAddr = ":8080"
	}

	// Initialize AGFS client if API URL is provided
	if *agfsAPIURL != "" {
		agfsClient = agfs.NewClient(*agfsAPIURL)
		agfsUploadPath = *agfsPath
		log.Printf("AGFS client initialized with API URL: %s", *agfsAPIURL)
		if agfsUploadPath != "" {
			log.Printf("AGFS upload path: %s", agfsUploadPath)
		}
	} else {
		log.Printf("AGFS API URL not provided, files will only be saved locally")
	}

	http.HandleFunc("/audio", handlePostAudio)
	log.Printf("Server starting on %s...", serverAddr)
	log.Fatal(http.ListenAndServe(serverAddr, nil))
}
