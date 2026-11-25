package workerexecutor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"

	"videoarchive/internal/core/execution"
)

// The receipt is written after verification and before the media rename. Both
// the first publisher and a later fenced assignment hold the same output lock.
type publicationReceipt struct {
	SpecDigest string `json:"spec_digest"`
	OutputID   string `json:"output_id"`
	Token      uint64 `json:"fencing_token"`
	Checksum   string `json:"sha256"`
	Size       int64  `json:"size"`
}

var errPublicationConflict = errors.New("published output cannot be matched to verified bytes")

func (p *storagePublisher) receiptPath() string {
	id := sha256.Sum256([]byte(p.publishedRelative))
	return fmt.Sprintf(".archive-receipts/%x.json", id[:])
}

// The renderer discarded its attempt file on a synchronous failure. If the
// rename already took effect (e.g. a directory sync failed), keep the receipt
// so a later attempt can verify and reuse the published bytes.
func (p *storagePublisher) discardUnpublishedReceipt() error {
	if p.receipt.Size == 0 {
		return nil
	}
	exists, err := p.root.Exists(p.publishedRelative)
	if exists || err != nil {
		return err
	}
	err = p.root.Remove(p.receiptPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (p *storagePublisher) recordBeforePublish() error {
	r := p.receipt
	if r.Size <= 0 || len(r.Checksum) != 64 {
		return errPublicationConflict
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	file := p.receiptPath()
	if err := p.root.MkdirAll(path.Dir(file), 0700); err != nil {
		return err
	}
	defer p.root.Remove(file + ".partial")
	if err := p.root.WriteFile(file+".partial", payload, 0600); err != nil {
		return err
	}
	return p.root.Rename(file+".partial", file)
}

// resume returns a verified output or leaves a fresh, unpublished target to its engine.
// Existing files with absent, stale or contradictory provenance require review.
func (p *storagePublisher) resume(spec execution.StepExecutionSpec, target execution.OutputTarget, token uint64) (string, error) {
	if token == 0 {
		return "", ErrProductionExecution
	}
	p.receipt = publicationReceipt{SpecDigest: spec.Digest, OutputID: target.ID, Token: token}
	file := p.receiptPath()
	exists, err := p.root.Exists(file)
	if err != nil {
		return "", err
	}
	published, err := p.root.Exists(p.publishedRelative)
	if err != nil {
		return "", err
	}
	if !exists {
		if published {
			return "", errPublicationConflict
		}
		return "", nil
	}
	reader, err := p.root.Open(file)
	if err != nil {
		return "", err
	}
	payload, err := io.ReadAll(io.LimitReader(reader, 8193))
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		return "", errors.Join(err, closeErr)
	}
	var prior publicationReceipt
	if len(payload) > 8192 || json.Unmarshal(payload, &prior) != nil || prior.SpecDigest != spec.Digest || prior.OutputID != target.ID || prior.Token == 0 || prior.Token > token || prior.Size <= 0 || len(prior.Checksum) != 64 {
		return "", errPublicationConflict
	}
	if _, err := hex.DecodeString(prior.Checksum); err != nil {
		return "", errPublicationConflict
	}
	candidate := p.publishedRelative
	if !published {
		candidate = temporaryOutputPathForToken(p.publishedRelative, prior.Token)
	}
	size, sum, err := p.root.FileSHA256(candidate)
	if err != nil || size != prior.Size || sum != prior.Checksum {
		return "", errors.Join(errPublicationConflict, err)
	}
	if !published {
		if err := p.root.Rename(candidate, p.publishedRelative); err != nil {
			return "", err
		}
	}
	return sum, nil
}

func attachPublishReferences(refs []string, outputs []execution.StepOutput, spec execution.StepExecutionSpec, reused map[string]bool) []string {
	for _, output := range outputs {
		refs = append(refs, execution.PublicationReference(spec.Digest, output, reused[output.ID]))
	}
	return refs
}
