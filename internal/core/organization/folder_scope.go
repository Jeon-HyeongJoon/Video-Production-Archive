package organization

import (
	"errors"
	"sort"
	"strings"
)

var (
	ErrInvalidFolderScope     = errors.New("folder scope is incomplete or invalid")
	ErrUnknownFolderSelection = errors.New("folder scope contains an unknown folder")
	ErrStaleDiscoveryRevision = errors.New("folder discovery revision is stale")
)

// FolderScope is the immutable set of folder identities selected from one
// discovery revision. Selecting all folders snapshots the identities that
// existed in that revision; folders discovered later are never implied.
type FolderScope struct {
	discoveryRevision uint64
	folderIDs         []string
}

type FixFolderScopeRequest struct {
	DiscoveryRevision   uint64
	DiscoveredFolderIDs []string
	SelectedFolderIDs   []string
	SelectAll           bool
}

func FixFolderScope(request FixFolderScopeRequest) (FolderScope, error) {
	if request.DiscoveryRevision == 0 {
		return FolderScope{}, ErrInvalidFolderScope
	}
	discovered, ok := normalizeFolderIDs(request.DiscoveredFolderIDs)
	if !ok || len(discovered) == 0 {
		return FolderScope{}, ErrInvalidFolderScope
	}
	if request.SelectAll {
		if len(request.SelectedFolderIDs) != 0 {
			return FolderScope{}, ErrInvalidFolderScope
		}
		return FolderScope{discoveryRevision: request.DiscoveryRevision, folderIDs: discovered}, nil
	}
	selected, ok := normalizeFolderIDs(request.SelectedFolderIDs)
	if !ok || len(selected) == 0 {
		return FolderScope{}, ErrInvalidFolderScope
	}
	known := make(map[string]struct{}, len(discovered))
	for _, id := range discovered {
		known[id] = struct{}{}
	}
	for _, id := range selected {
		if _, exists := known[id]; !exists {
			return FolderScope{}, ErrUnknownFolderSelection
		}
	}
	return FolderScope{discoveryRevision: request.DiscoveryRevision, folderIDs: selected}, nil
}

func (scope FolderScope) DiscoveryRevision() uint64 {
	return scope.discoveryRevision
}

func (scope FolderScope) FolderIDs() []string {
	return append([]string(nil), scope.folderIDs...)
}

func (scope FolderScope) ValidateStart(currentDiscoveryRevision uint64) error {
	if scope.discoveryRevision == 0 || len(scope.folderIDs) == 0 {
		return ErrInvalidFolderScope
	}
	if currentDiscoveryRevision != scope.discoveryRevision {
		return ErrStaleDiscoveryRevision
	}
	return nil
}

func (scope FolderScope) Includes(folderID string) bool {
	folderID = strings.TrimSpace(folderID)
	index := sort.SearchStrings(scope.folderIDs, folderID)
	return index < len(scope.folderIDs) && scope.folderIDs[index] == folderID
}

func normalizeFolderIDs(source []string) ([]string, bool) {
	values := make([]string, 0, len(source))
	seen := make(map[string]struct{}, len(source))
	for _, sourceID := range source {
		id := strings.TrimSpace(sourceID)
		if id == "" {
			return nil, false
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, false
		}
		seen[id] = struct{}{}
		values = append(values, id)
	}
	sort.Strings(values)
	return values, true
}
