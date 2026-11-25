package whispercpp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"unicode/utf8"

	"videoarchive/internal/core/archive"
	"videoarchive/internal/core/transcription"
)

// Checkpoints are private retry data scoped to a publication target. They
// contain only normalized chunk content and never constitute a Transcript.
type chunkCheckpoints struct {
	root *archive.StorageRoot
	dir  string
	key  string
}

func checkpointDirectory(target string) string {
	return path.Join("staging", ".archive-stt", fmt.Sprintf("%x", sha256.Sum256([]byte(target))))
}

func newChunkCheckpoints(request Request, version string, profile transcription.Profile, chunks []speechChunk) (*chunkCheckpoints, error) {
	if len(chunks) < 2 || request.InputRoot == nil || request.ModelRoot == nil || request.OutputRoot == nil {
		return nil, nil
	}
	modelPath, err := request.ModelRoot.ExternalProcessPath(request.ModelRelativePath)
	if err != nil || filepath.Clean(modelPath) != filepath.Clean(request.ResolvedModelPath) {
		return nil, ErrInvalidRequest
	}
	_, modelSum, err := request.ModelRoot.FileSHA256(request.ModelRelativePath)
	if err != nil {
		return nil, err
	}

	// Bump the format whenever engine options or normalization semantics change.
	identity, err := json.Marshal(struct {
		Format                   int
		ArtifactID, OriginalID   string
		AudioSHA256, ModelSHA256 string
		ProcessorVersion         string
		Profile                  transcription.Profile
		Plan                     []speechChunk
	}{
		Format: 1, ArtifactID: request.AudioArtifact.Artifact.ID, OriginalID: request.AudioArtifact.Artifact.Origin.OriginalAssetID,
		AudioSHA256: request.AudioArtifact.Artifact.Checksum, ModelSHA256: modelSum, ProcessorVersion: version, Profile: profile, Plan: chunks,
	})
	if err != nil {
		return nil, err
	}
	return &chunkCheckpoints{
		root: request.OutputRoot,
		dir:  checkpointDirectory(request.ArtifactPath),
		key:  fmt.Sprintf("%x", sha256.Sum256(identity)),
	}, nil
}

type checkpointRecord struct {
	Content  json.RawMessage `json:"content"`
	Checksum string          `json:"checksum"`
}

// Bind the bytes to both their plan and ordinal. A misplaced, torn or old
// record must never become a cue in another chunk.
func (store *chunkCheckpoints) checksum(index int, content []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s/%d/", store.key, index)
	h.Write(content)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (store *chunkCheckpoints) file(index int) string {
	return path.Join(store.dir, fmt.Sprintf("%d.json", index))
}

func (store *chunkCheckpoints) load(index int, chunk speechChunk) (transcription.Content, bool) {
	if store == nil {
		return transcription.Content{}, false
	}
	data, err := store.root.ReadFile(store.file(index))
	if err != nil || !utf8.Valid(data) {
		return transcription.Content{}, false
	}
	var record checkpointRecord
	if json.Unmarshal(data, &record) != nil || store.checksum(index, record.Content) != record.Checksum {
		return transcription.Content{}, false
	}
	var content transcription.Content
	if json.Unmarshal(record.Content, &content) != nil || content.Language == "" {
		return transcription.Content{}, false
	}
	if n := len(content.Segments); n > 0 {
		if content.Segments[0].StartMillis < chunk.LogicalStartMillis || content.Segments[n-1].EndMillis > chunk.LogicalEndMillis {
			return transcription.Content{}, false
		}
		verified, err := transcription.NewContent(content.Language, content.Segments)
		return verified, err == nil
	}
	return content, true
}

// A checkpoint is optional. If storage is unavailable the current attempt
// still has the verified in-memory content and a retry can rerun the engine.
func (store *chunkCheckpoints) save(index int, content transcription.Content) {
	if store == nil {
		return
	}
	payload, err := json.Marshal(content)
	if err != nil {
		return
	}
	record, err := json.Marshal(checkpointRecord{Content: payload, Checksum: store.checksum(index, payload)})
	if err != nil || store.root.MkdirAll(store.dir, 0700) != nil {
		return
	}
	name := store.file(index)
	tmp := path.Join(store.dir, rand.Text()+".tmp")
	defer store.root.Remove(tmp)
	if store.root.WriteFile(tmp, record, 0600) == nil {
		_ = store.root.Rename(tmp, name)
	}
}

func (store *chunkCheckpoints) clear() {
	if store != nil {
		_ = store.root.RemoveAll(store.dir)
	}
}
