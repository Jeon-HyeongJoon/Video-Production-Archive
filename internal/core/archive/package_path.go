package archive

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrInvalidPackagePresentation = errors.New("archive package presentation is invalid")

type PackageLayoutVersion uint8

const (
	PackageLayoutDateRevisionTree    PackageLayoutVersion = 1
	PackageLayoutFlatResultFolder    PackageLayoutVersion = 2
	PackageLayoutCompactResultFolder PackageLayoutVersion = 3
)

func (version PackageLayoutVersion) Valid() bool {
	return version == PackageLayoutDateRevisionTree || version == PackageLayoutFlatResultFolder ||
		version == PackageLayoutCompactResultFolder
}

var packageIdentityPattern = regexp.MustCompile(`^VP-([0-9]{4})([0-9]{2})([0-9]{2})-[A-Za-z0-9._-]+$`)

type PackagePresentation struct {
	ArchiveDate      string
	MediaFolder      string
	DisplayName      string
	VideoFileName    string
	PackageDirectory string
}

// FlatPackagePreview describes the human-visible part of the compact layout
// before inspection has produced the immutable package identity and decided
// whether multiple source videos stay separate or become one sequence.
type FlatPackagePreview struct {
	ArchiveDate                     string
	MinimumResultCount              int
	MaximumResultCount              int
	CombinedDirectoryPattern        string
	CombinedVideoFileName           string
	FirstIndividualDirectoryPattern string
	FirstIndividualVideoFileName    string
}

// PreviewFlatPackage projects only root-relative names. XXXXXXXXXXXX occupies
// the twelve hexadecimal characters that the immutable package identity adds
// after classification; it is deliberately not a filesystem reservation.
func PreviewFlatPackage(archiveDate, mediaFolder string, originalNames []string, sourceCount int) (FlatPackagePreview, error) {
	archiveDate = strings.TrimSpace(archiveDate)
	if _, err := time.Parse("2006-01-02", archiveDate); err != nil || sourceCount <= 0 || sourceCount < len(originalNames) {
		return FlatPackagePreview{}, ErrInvalidPackagePresentation
	}
	folder, ok := normalizePackageRelative(mediaFolder)
	if !ok {
		return FlatPackagePreview{}, ErrInvalidPackagePresentation
	}
	names := make([]string, 0, len(originalNames))
	for _, originalName := range originalNames {
		name := strings.TrimSpace(originalName)
		if name == "" || name != path.Base(strings.ReplaceAll(name, "\\", "/")) {
			return FlatPackagePreview{}, ErrInvalidPackagePresentation
		}
		names = append(names, name)
	}
	firstName := path.Base(folder) + ".mp4"
	if len(names) > 0 {
		firstName = names[0]
	}
	individualStem := safeWindowsFileStem(strings.TrimSuffix(firstName, path.Ext(firstName)))
	if individualStem == "" {
		individualStem = "video"
	}
	combinedStem := individualStem
	if sourceCount > 1 {
		combinedStem = safeWindowsFileStem(path.Base(folder))
		if combinedStem == "" {
			combinedStem = "video"
		}
	}
	directoryPattern := func(stem string) string {
		return safeWindowsFileStem(archiveDate+"_"+stem) + "__XXXXXXXXXXXX"
	}
	return FlatPackagePreview{
		ArchiveDate: archiveDate, MinimumResultCount: 1, MaximumResultCount: sourceCount,
		CombinedDirectoryPattern: directoryPattern(combinedStem), CombinedVideoFileName: combinedStem + ".mp4",
		FirstIndividualDirectoryPattern: directoryPattern(individualStem), FirstIndividualVideoFileName: individualStem + ".mp4",
	}, nil
}

func PresentPackage(packageID, mediaFolder string, originalNames []string) (PackagePresentation, error) {
	directory, archiveDate, err := packageDirectory(packageID)
	if err != nil {
		return PackagePresentation{}, err
	}
	folder, ok := normalizePackageRelative(mediaFolder)
	if !ok || len(originalNames) == 0 {
		return PackagePresentation{}, ErrInvalidPackagePresentation
	}
	names := make([]string, 0, len(originalNames))
	for _, originalName := range originalNames {
		name := strings.TrimSpace(originalName)
		if name == "" || name != path.Base(strings.ReplaceAll(name, "\\", "/")) {
			return PackagePresentation{}, ErrInvalidPackagePresentation
		}
		names = append(names, name)
	}
	label := names[0]
	stem := strings.TrimSuffix(label, path.Ext(label))
	if len(names) > 1 {
		label = path.Base(folder)
		stem = label
	}
	stem = safeWindowsFileStem(stem)
	if stem == "" {
		stem = "video"
	}
	return PackagePresentation{
		ArchiveDate: archiveDate, MediaFolder: folder,
		DisplayName:   archiveDate + " · " + folder + " · " + label,
		VideoFileName: stem + ".mp4", PackageDirectory: directory,
	}, nil
}

func PresentFlatPackage(packageID, mediaFolder string, originalNames []string) (PackagePresentation, error) {
	presentation, err := PresentPackage(packageID, mediaFolder, originalNames)
	if err != nil {
		return PackagePresentation{}, err
	}
	prefix := "VP-" + strings.ReplaceAll(presentation.ArchiveDate, "-", "") + "-"
	suffix := strings.TrimPrefix(strings.TrimSpace(packageID), prefix)
	if suffix == "" || suffix == packageID {
		return PackagePresentation{}, ErrInvalidPackagePresentation
	}
	if utf8.RuneCountInString(suffix) > 12 {
		runes := []rune(suffix)
		suffix = string(runes[len(runes)-12:])
	}
	resultStem := strings.TrimSuffix(presentation.VideoFileName, path.Ext(presentation.VideoFileName))
	if utf8.RuneCountInString(resultStem) > 48 {
		runes := []rune(resultStem)
		resultStem = string(runes[:48])
	}
	directoryName := safeWindowsFileStem(presentation.ArchiveDate + "_" + resultStem + "__" + suffix)
	if directoryName == "" {
		return PackagePresentation{}, ErrInvalidPackagePresentation
	}
	presentation.PackageDirectory = directoryName
	return presentation, nil
}

func PackageDirectory(packageID string) (string, error) {
	directory, _, err := packageDirectory(packageID)
	return directory, err
}

func PackageRevisionDirectory(packageID string, revision uint64) (string, error) {
	if revision == 0 {
		return "", ErrInvalidPackagePresentation
	}
	directory, err := PackageDirectory(packageID)
	if err != nil {
		return "", err
	}
	return path.Join(directory, "revisions", fmt.Sprintf("%06d", revision)), nil
}

func packageDirectory(packageID string) (string, string, error) {
	packageID = strings.TrimSpace(packageID)
	parts := packageIdentityPattern.FindStringSubmatch(packageID)
	if len(parts) != 4 {
		return "", "", ErrInvalidPackagePresentation
	}
	date := parts[1] + parts[2] + parts[3]
	parsed, err := time.Parse("20060102", date)
	if err != nil {
		return "", "", ErrInvalidPackagePresentation
	}
	return path.Join("packages", parts[1], parts[2], parts[3], packageID), parsed.Format("2006-01-02"), nil
}

func normalizePackageRelative(value string) (string, bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(normalized, "/") ||
		strings.HasPrefix(normalized, "//") || (len(normalized) >= 3 && normalized[1] == ':' && normalized[2] == '/') {
		return "", false
	}
	return clean, true
}

func safeWindowsFileStem(value string) string {
	var builder strings.Builder
	lastReplacement := false
	for _, character := range strings.TrimSpace(value) {
		invalid := character < 32 || strings.ContainsRune(`<>:"/\|?*`, character)
		if invalid {
			if !lastReplacement {
				builder.WriteRune('_')
			}
			lastReplacement = true
			continue
		}
		builder.WriteRune(character)
		lastReplacement = false
	}
	result := strings.Trim(builder.String(), " .")
	if result == "" {
		return ""
	}
	if windowsReservedName(result) {
		result = "_" + result
	}
	for utf8.RuneCountInString(result) > 80 {
		_, size := utf8.DecodeLastRuneInString(result)
		result = result[:len(result)-size]
	}
	return strings.TrimRight(result, " .")
}

func windowsReservedName(value string) bool {
	upper := strings.ToUpper(strings.TrimSpace(value))
	if upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" {
		return true
	}
	if len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) {
		return upper[3] >= '1' && upper[3] <= '9'
	}
	return false
}
