package organization

import (
	"errors"
	"reflect"
	"testing"
)

// Requirement: ORG-011
func TestFolderScopeFreezesSelectedDiscoveryRevisionAndRejectsStaleStart(t *testing.T) {
	discoveredAtR1 := []string{"FOLDER-B", "FOLDER-A"}
	all, err := FixFolderScope(FixFolderScopeRequest{
		DiscoveryRevision:   1,
		DiscoveredFolderIDs: discoveredAtR1,
		SelectAll:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if all.DiscoveryRevision() != 1 || !reflect.DeepEqual(all.FolderIDs(), []string{"FOLDER-A", "FOLDER-B"}) ||
		all.Includes("FOLDER-C") {
		t.Fatalf("all selection did not freeze the R1 folder identities: revision=%d folders=%v", all.DiscoveryRevision(), all.FolderIDs())
	}
	if err := all.ValidateStart(2); !errors.Is(err, ErrStaleDiscoveryRevision) {
		t.Fatalf("R1 selection was accepted after discovery changed to R2: %v", err)
	}
	if !reflect.DeepEqual(all.FolderIDs(), []string{"FOLDER-A", "FOLDER-B"}) {
		t.Fatalf("later discovery implicitly changed the frozen all selection: %v", all.FolderIDs())
	}

	selected, err := FixFolderScope(FixFolderScopeRequest{
		DiscoveryRevision:   1,
		DiscoveredFolderIDs: discoveredAtR1,
		SelectedFolderIDs:   []string{"FOLDER-B"},
	})
	if err != nil || selected.ValidateStart(1) != nil || !reflect.DeepEqual(selected.FolderIDs(), []string{"FOLDER-B"}) {
		t.Fatalf("explicit R1 subset was not fixed: scope=%v err=%v", selected.FolderIDs(), err)
	}
	if _, err := FixFolderScope(FixFolderScopeRequest{
		DiscoveryRevision:   1,
		DiscoveredFolderIDs: discoveredAtR1,
		SelectedFolderIDs:   []string{"FOLDER-C"},
	}); !errors.Is(err, ErrUnknownFolderSelection) {
		t.Fatalf("unknown folder selection was accepted: %v", err)
	}
}
