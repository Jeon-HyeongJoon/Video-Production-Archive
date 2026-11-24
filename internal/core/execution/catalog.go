package execution

import "time"

type PublishedPackage struct {
	PackageID      string `json:"package_id"`
	Revision       uint64 `json:"revision"`
	ManifestDigest string `json:"manifest_digest"`
}

type CatalogPublication struct {
	ArchiveStorageID string             `json:"archive_storage_id"`
	ArchiveRunID     string             `json:"archive_run_id"`
	Packages         []PublishedPackage `json:"packages"`
	CatalogSHA256    string             `json:"catalog_sha256"`
}

func (p CatalogPublication) Matches(run, storage string, o StepOutput) bool {
	if run == "" || storage == "" || len(p.Packages) == 0 || p.ArchiveRunID != run || p.ArchiveStorageID != storage || o.StorageID != storage || o.Kind != OutputArchiveCatalog || p.CatalogSHA256 != o.Checksum || !sha256Pattern.MatchString(o.Checksum) {
		return false
	}
	seen := map[string]bool{}
	for _, pkg := range p.Packages {
		if pkg.PackageID == "" || pkg.Revision == 0 || !sha256Pattern.MatchString(pkg.ManifestDigest) || seen[pkg.PackageID] {
			return false
		}
		seen[pkg.PackageID] = true
	}
	return true
}

type CatalogVerification struct {
	ArchiveStorageID   string    `json:"archive_storage_id"`
	State              string    `json:"state"`
	ReportID           string    `json:"report_id,omitempty"`
	ReportSHA256       string    `json:"report_sha256,omitempty"`
	ObservedAt         time.Time `json:"observed_at,omitzero"`
	CatalogSHA256      string    `json:"catalog_sha256,omitempty"`
	LegacyPackages     int       `json:"legacy_packages,omitempty"`
	CurrentPackages    int       `json:"current_packages"`
	MismatchedPackages int       `json:"mismatched_packages"`
	UnresolvedVideos   int       `json:"unresolved_videos"`
	NextCheck          string    `json:"next_check"`
}

func (v CatalogVerification) Matches(storage string, o StepOutput) bool {
	if storage == "" || v.ArchiveStorageID != storage || o.Kind != OutputArchiveCatalogVerification || v.ReportID != o.ID || v.ReportSHA256 != o.Checksum || !sha256Pattern.MatchString(o.Checksum) || v.ObservedAt.IsZero() || (v.CatalogSHA256 != "" && !sha256Pattern.MatchString(v.CatalogSHA256)) || v.LegacyPackages < 0 || v.CurrentPackages < 0 || v.MismatchedPackages < 0 || v.UnresolvedVideos < 0 {
		return false
	}
	switch v.State {
	case "CURRENT":
		return (v.CurrentPackages > 0 || v.LegacyPackages > 0) && v.MismatchedPackages == 0 && v.UnresolvedVideos == 0 && v.NextCheck == "NONE"
	case "EMPTY":
		return v.CurrentPackages+v.MismatchedPackages+v.UnresolvedVideos == 0 && v.NextCheck == "VERIFY_AFTER_PUBLICATION"
	case "MISMATCH":
		return v.MismatchedPackages > 0 && v.UnresolvedVideos == 0 && v.NextCheck == "REVIEW_CATALOG_DIFFERENCES"
	case "INCOMPLETE":
		return v.NextCheck == "REVIEW_VERIFICATION_COVERAGE"
	default:
		return false
	}
}
