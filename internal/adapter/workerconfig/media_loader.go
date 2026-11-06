package workerconfig

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"videoarchive/internal/core/transcription"
)

type mediaDocument struct {
	SchemaVersion uint32 `yaml:"schema_version"`
	Media         struct {
		ID           string `yaml:"id"`
		StorageRoots []struct {
			StorageID string `yaml:"storage_id"`
			Root      string `yaml:"root"`
		} `yaml:"storage_roots"`
		Tools struct {
			FFmpeg     mediaLocalToolDocument `yaml:"ffmpeg"`
			FFprobe    mediaLocalToolDocument `yaml:"ffprobe"`
			WhisperCPP *struct {
				Executable        string `yaml:"executable"`
				Version           string `yaml:"version"`
				ModelStorageID    string `yaml:"model_storage_id"`
				ModelRelativePath string `yaml:"model_relative_path"`
				Profile           struct {
					ID          string `yaml:"id"`
					Version     uint32 `yaml:"version"`
					ModelID     string `yaml:"model_id"`
					Language    string `yaml:"language"`
					ThreadCount int    `yaml:"thread_count"`
				} `yaml:"profile"`
			} `yaml:"whispercpp"`
		} `yaml:"tools"`
	} `yaml:"media"`
}

type mediaLocalToolDocument struct {
	Executable string `yaml:"executable"`
	Version    string `yaml:"version"`
}

// LoadMedia loads the standalone MediaOperation configuration. It deliberately
// contains no Control URL, service credential, heartbeat, or queue settings.
func LoadMedia(filePath string) (Configuration, error) {
	file, err := os.Open(strings.TrimSpace(filePath))
	if err != nil {
		return Configuration{}, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var source mediaDocument
	if err := decoder.Decode(&source); err != nil {
		return Configuration{}, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Configuration{}, fmt.Errorf("%w: configuration must contain one YAML document", ErrInvalidConfiguration)
	}
	return normalizeMedia(source)
}

func normalizeMedia(source mediaDocument) (Configuration, error) {
	executorID := strings.TrimSpace(source.Media.ID)
	if source.SchemaVersion != 1 || executorID == "" || len(source.Media.StorageRoots) == 0 {
		return Configuration{}, ErrInvalidConfiguration
	}
	seen := make(map[string]bool, len(source.Media.StorageRoots))
	storageIDs := make([]string, 0, len(source.Media.StorageRoots))
	storageRoots := make([]StorageRootConfiguration, 0, len(source.Media.StorageRoots))
	for _, raw := range source.Media.StorageRoots {
		storageID := strings.TrimSpace(raw.StorageID)
		root := strings.TrimSpace(raw.Root)
		if storageID == "" || seen[storageID] || root == "" || !filepath.IsAbs(root) {
			return Configuration{}, ErrInvalidConfiguration
		}
		seen[storageID] = true
		storageIDs = append(storageIDs, storageID)
		storageRoots = append(storageRoots, StorageRootConfiguration{StorageID: storageID, Root: filepath.Clean(root)})
	}
	ffmpeg, err := normalizeLocalTool(source.Media.Tools.FFmpeg.Executable, source.Media.Tools.FFmpeg.Version)
	if err != nil {
		return Configuration{}, err
	}
	ffprobe, err := normalizeLocalTool(source.Media.Tools.FFprobe.Executable, source.Media.Tools.FFprobe.Version)
	if err != nil {
		return Configuration{}, err
	}
	var whisper *WhisperTool
	if source.Media.Tools.WhisperCPP != nil {
		raw := source.Media.Tools.WhisperCPP
		localTool, toolErr := normalizeLocalTool(raw.Executable, raw.Version)
		modelStorageID := strings.TrimSpace(raw.ModelStorageID)
		modelRelativePath, relativeOK := normalizeRelativePath(raw.ModelRelativePath)
		profile, profileErr := transcription.NewProfile(transcription.Profile{
			ID: raw.Profile.ID, Version: raw.Profile.Version, ModelID: raw.Profile.ModelID,
			Language: raw.Profile.Language, ThreadCount: raw.Profile.ThreadCount,
		})
		if toolErr != nil || !seen[modelStorageID] || !relativeOK || profileErr != nil {
			return Configuration{}, ErrInvalidConfiguration
		}
		whisper = &WhisperTool{
			LocalTool: localTool, ModelStorageID: modelStorageID, ModelRelativePath: modelRelativePath, Profile: profile,
		}
	}
	return Configuration{
		SchemaVersion: source.SchemaVersion, ID: executorID,
		StorageRoots: storageRoots, AccessibleStorageIDs: storageIDs,
		Tools: MediaTools{FFmpeg: ffmpeg, FFprobe: ffprobe, WhisperCPP: whisper},
	}, nil
}
