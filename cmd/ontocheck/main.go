package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"videoarchive/internal/repositorycheck"
)

type ontology struct {
	Version              int            `json:"version"`
	GlobalForbiddenWords []string       `json:"global_forbidden_words"`
	Layers               []vocabulary   `json:"layers"`
	Kinds                []kind         `json:"kinds"`
	RelationTypes        []vocabulary   `json:"relation_types"`
	FollowUpTypes        []vocabulary   `json:"follow_up_types"`
	Terms                []term         `json:"terms"`
	Open                 []openQuestion `json:"open"`
	FollowUps            []followUp     `json:"follow_ups"`
}

type followUp struct {
	Term string `json:"term"`
	Type string `json:"type"`
	From string `json:"from"`
	To   string `json:"to"`
}

type vocabulary struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type kind struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	HasValues   bool   `json:"has_values"`
	CodeSuffix  string `json:"code_suffix"`
}

type name struct {
	KO string `json:"ko"`
	EN string `json:"en"`
}

type forbidden struct {
	Words []string `json:"words"`
	Code  []string `json:"code"`
}

type relation struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

type term struct {
	ID         string       `json:"id"`
	Layer      string       `json:"layer"`
	Kind       string       `json:"kind"`
	Name       name         `json:"name"`
	CodeName   string       `json:"code_name"`
	Definition string       `json:"definition"`
	Values     []vocabulary `json:"values"`
	Includes   []string     `json:"includes"`
	Rules      []string     `json:"rules"`
	Forbidden  forbidden    `json:"forbidden"`
	Relations  []relation   `json:"relations"`
	Sources    []string     `json:"sources"`
	Decided    string       `json:"decided"`
}

type sense struct {
	Meaning string   `json:"meaning"`
	Where   []string `json:"where"`
}

type openQuestion struct {
	ID       string   `json:"id"`
	Words    []string `json:"words"`
	Senses   []sense  `json:"senses"`
	Proposal string   `json:"proposal"`
}

var (
	identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	valuePattern      = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	datePattern       = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

func main() {
	root, err := repositoryRoot()
	check(err)
	payload, err := os.ReadFile(filepath.Join(root, "ontology", "terms.json"))
	check(err)

	var entries ontology
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	check(decoder.Decode(&entries))

	problems := validate(entries)
	codeProblems, err := forbiddenIdentifiers(root, entries.Terms)
	check(err)
	problems = append(problems, codeProblems...)
	wordProblems, err := globallyForbiddenWords(root, entries.GlobalForbiddenWords)
	check(err)
	problems = append(problems, wordProblems...)
	architectureProblems, err := sharedVocabularyArchitecture(root)
	check(err)
	problems = append(problems, architectureProblems...)
	valueProblems, err := sharedVocabularyValues(root, entries.Terms)
	check(err)
	problems = append(problems, valueProblems...)
	if len(problems) > 0 {
		sort.Strings(problems)
		check(fmt.Errorf("ontology failed:\n- %s", strings.Join(problems, "\n- ")))
	}
	fmt.Printf("ontology ok: %d terms, %d open questions, %d follow-ups\n", len(entries.Terms), len(entries.Open), len(entries.FollowUps))
}

func repositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("go.mod not found")
		}
		directory = parent
	}
}

func validate(entries ontology) []string {
	var problems []string
	if entries.Version != 1 {
		problems = append(problems, fmt.Sprintf("unsupported ontology version %d", entries.Version))
	}
	seenGlobalWords := make(map[string]bool, len(entries.GlobalForbiddenWords))
	for _, word := range entries.GlobalForbiddenWords {
		if strings.TrimSpace(word) == "" || seenGlobalWords[word] {
			problems = append(problems, fmt.Sprintf("invalid or duplicate globally forbidden word %q", word))
		}
		seenGlobalWords[word] = true
	}
	layers := vocabularySet("layer", entries.Layers, &problems)
	kinds := make(map[string]kind, len(entries.Kinds))
	kindVocabulary := make([]vocabulary, 0, len(entries.Kinds))
	for _, k := range entries.Kinds {
		kinds[k.ID] = k
		kindVocabulary = append(kindVocabulary, vocabulary{ID: k.ID, Description: k.Description})
	}
	vocabularySet("kind", kindVocabulary, &problems)
	relationTypes := vocabularySet("relation type", entries.RelationTypes, &problems)
	followUpTypes := vocabularySet("follow-up type", entries.FollowUpTypes, &problems)

	termIDs := make(map[string]bool, len(entries.Terms))
	owners := make(map[string]string)
	for _, t := range entries.Terms {
		if !identifierPattern.MatchString(t.ID) {
			problems = append(problems, fmt.Sprintf("invalid term id %q", t.ID))
		}
		if termIDs[t.ID] {
			problems = append(problems, fmt.Sprintf("duplicate term %s", t.ID))
		}
		termIDs[t.ID] = true
		if !layers[t.Layer] {
			problems = append(problems, fmt.Sprintf("%s has unknown layer %q", t.ID, t.Layer))
		}
		if k, known := kinds[t.Kind]; !known {
			problems = append(problems, fmt.Sprintf("%s has unknown kind %q", t.ID, t.Kind))
		} else {
			problems = append(problems, kindProblems(t, k)...)
		}
		if t.Name.KO == "" || t.Name.EN == "" || t.Definition == "" || len(t.Sources) == 0 {
			problems = append(problems, fmt.Sprintf("%s has an incomplete definition", t.ID))
		}
		if !datePattern.MatchString(t.Decided) {
			problems = append(problems, fmt.Sprintf("%s has invalid decided date %q", t.ID, t.Decided))
		}
		for _, word := range []string{t.Name.KO, t.Name.EN} {
			if word == "" {
				continue
			}
			if owner, taken := owners[word]; taken && owner != t.ID {
				problems = append(problems, fmt.Sprintf("%q names both %s and %s", word, owner, t.ID))
			}
			owners[word] = t.ID
		}
	}

	for _, t := range entries.Terms {
		for _, r := range t.Relations {
			if !relationTypes[r.Type] {
				problems = append(problems, fmt.Sprintf("%s has unknown relation type %q", t.ID, r.Type))
			}
			if !termIDs[r.Target] {
				problems = append(problems, fmt.Sprintf("%s relates to unknown term %s", t.ID, r.Target))
			}
		}
		for _, word := range append(append([]string(nil), t.Forbidden.Words...), t.Forbidden.Code...) {
			if word == t.Name.KO || word == t.Name.EN || word == t.CodeName {
				problems = append(problems, fmt.Sprintf("%s forbids its own name %q", t.ID, word))
			}
		}
	}

	for _, f := range entries.FollowUps {
		if !termIDs[f.Term] {
			problems = append(problems, fmt.Sprintf("follow-up %q -> %q belongs to unknown term %s", f.From, f.To, f.Term))
		}
		if !followUpTypes[f.Type] {
			problems = append(problems, fmt.Sprintf("follow-up for %s has unknown type %q", f.Term, f.Type))
		}
		if f.From == "" || f.To == "" {
			problems = append(problems, fmt.Sprintf("follow-up for %s needs from and to", f.Term))
		}
	}

	openIDs := make(map[string]bool, len(entries.Open))
	for _, q := range entries.Open {
		if !identifierPattern.MatchString(q.ID) {
			problems = append(problems, fmt.Sprintf("invalid open question id %q", q.ID))
		}
		if openIDs[q.ID] {
			problems = append(problems, fmt.Sprintf("duplicate open question %s", q.ID))
		}
		openIDs[q.ID] = true
		if termIDs[q.ID] {
			problems = append(problems, fmt.Sprintf("open question %s is already a decided term", q.ID))
		}
		if len(q.Words) == 0 || len(q.Senses) < 2 {
			problems = append(problems, fmt.Sprintf("open question %s needs words and at least two senses", q.ID))
		}
	}
	return problems
}

// globallyForbiddenWords scans prose where the current vocabulary is meant to
// be used. Per-term forbidden.words remain contextual aliases and are not safe
// to enforce with an unscoped text search.
func globallyForbiddenWords(root string, words []string) ([]string, error) {
	if len(words) == 0 {
		return nil, nil
	}
	var problems []string
	err := repositorycheck.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for lineNumber, line := range strings.Split(string(payload), "\n") {
			for _, word := range words {
				if strings.Contains(line, word) {
					problems = append(problems, fmt.Sprintf("%s:%d uses globally forbidden word %q", filepath.ToSlash(relative), lineNumber+1, word))
				}
			}
		}
		return nil
	})
	return problems, err
}

func sharedVocabularyArchitecture(root string) ([]string, error) {
	canonicalTypes := map[string]bool{
		"ActorType": true, "UserRole": true, "WaitReason": true, "ProcessingKind": true,
		"ResourceClass": true, "ExecutorKind": true, "ApprovalDecision": true, "ContentReviewDecision": true,
	}
	fieldNames := map[string]bool{
		"ActorType": true, "UserRole": true, "WaitReason": true, "ProcessingKind": true,
		"ProcessingKinds": true, "RequiredProcessingKinds": true, "ResourceClass": true,
		"ExecutorKind": true, "FallbackExecutorKind": true, "ApprovalDecision": true,
		"ContentReviewDecision": true,
	}
	coreRoot := filepath.Join(root, "internal", "core")
	var problems []string
	err := filepath.WalkDir(coreRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		isVocabularyPackage := filepath.ToSlash(filepath.Dir(relative)) == "internal/core/vocabulary"
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.TypeSpec:
				if canonicalTypes[value.Name.Name] && !isVocabularyPackage && !value.Assign.IsValid() {
					line := fileSet.Position(value.Pos()).Line
					problems = append(problems, fmt.Sprintf("%s:%d redefines shared vocabulary type %s", filepath.ToSlash(relative), line, value.Name.Name))
				}
			case *ast.Field:
				if !containsBuiltinString(value.Type) {
					return true
				}
				for _, name := range value.Names {
					if fieldNames[name.Name] {
						line := fileSet.Position(name.Pos()).Line
						problems = append(problems, fmt.Sprintf("%s:%d stores shared vocabulary field %s as string", filepath.ToSlash(relative), line, name.Name))
					}
				}
			}
			return true
		})
		return nil
	})
	return problems, err
}

func containsBuiltinString(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if ok && identifier.Name == "string" {
			found = true
			return false
		}
		return !found
	})
	return found
}

func sharedVocabularyValues(root string, terms []term) ([]string, error) {
	path := filepath.Join(root, "internal", "core", "vocabulary", "vocabulary.go")
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		return nil, err
	}
	codeValues := make(map[string]map[string]bool)
	for _, declaration := range parsed.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok || generic.Tok != token.CONST {
			continue
		}
		for _, spec := range generic.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok || len(valueSpec.Values) != 1 {
				continue
			}
			typeName, ok := valueSpec.Type.(*ast.Ident)
			literal, literalOK := valueSpec.Values[0].(*ast.BasicLit)
			if !ok || !literalOK || literal.Kind != token.STRING {
				continue
			}
			value := strings.Trim(literal.Value, "\"")
			if codeValues[typeName.Name] == nil {
				codeValues[typeName.Name] = make(map[string]bool)
			}
			codeValues[typeName.Name][value] = true
		}
	}
	ontologyValues := make(map[string]map[string]bool)
	for _, term := range terms {
		if term.CodeName == "" || len(term.Values) == 0 {
			continue
		}
		values := make(map[string]bool, len(term.Values))
		for _, value := range term.Values {
			values[value.ID] = true
		}
		ontologyValues[term.CodeName] = values
	}
	var problems []string
	for typeName, values := range codeValues {
		decided, exists := ontologyValues[typeName]
		if !exists {
			problems = append(problems, fmt.Sprintf("shared vocabulary type %s has no ontology enum", typeName))
			continue
		}
		for value := range values {
			if !decided[value] {
				problems = append(problems, fmt.Sprintf("shared vocabulary %s code has value %s missing from ontology", typeName, value))
			}
		}
		for value := range decided {
			if !values[value] {
				problems = append(problems, fmt.Sprintf("shared vocabulary %s ontology has value %s missing from code", typeName, value))
			}
		}
	}
	return problems, nil
}

func kindProblems(t term, k kind) []string {
	var problems []string
	if k.HasValues && len(t.Values) == 0 {
		problems = append(problems, fmt.Sprintf("%s is a %s and needs values", t.ID, k.ID))
	}
	if !k.HasValues && len(t.Values) > 0 {
		problems = append(problems, fmt.Sprintf("%s is a %s, which does not take values", t.ID, k.ID))
	}
	seen := make(map[string]bool, len(t.Values))
	for _, value := range t.Values {
		if !valuePattern.MatchString(value.ID) {
			problems = append(problems, fmt.Sprintf("value %q of %s is not upper SNAKE_CASE", value.ID, t.ID))
		}
		if seen[value.ID] {
			problems = append(problems, fmt.Sprintf("duplicate value %s in %s", value.ID, t.ID))
		}
		if value.Description == "" {
			problems = append(problems, fmt.Sprintf("value %s of %s has no description", value.ID, t.ID))
		}
		seen[value.ID] = true
	}
	if k.CodeSuffix != "" && t.CodeName != "" && !strings.HasSuffix(t.CodeName, k.CodeSuffix) {
		problems = append(problems, fmt.Sprintf("%s code name %q must end with %q", t.ID, t.CodeName, k.CodeSuffix))
	}
	return problems
}

func vocabularySet(label string, entries []vocabulary, problems *[]string) map[string]bool {
	set := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !identifierPattern.MatchString(entry.ID) {
			*problems = append(*problems, fmt.Sprintf("invalid %s id %q", label, entry.ID))
		}
		if set[entry.ID] {
			*problems = append(*problems, fmt.Sprintf("duplicate %s %s", label, entry.ID))
		}
		if entry.Description == "" {
			*problems = append(*problems, fmt.Sprintf("%s %s has no description", label, entry.ID))
		}
		set[entry.ID] = true
	}
	return set
}

func forbiddenIdentifiers(root string, terms []term) ([]string, error) {
	owners := make(map[string]string)
	for _, t := range terms {
		for _, identifier := range t.Forbidden.Code {
			owners[identifier] = t.ID
		}
	}
	if len(owners) == 0 {
		return nil, nil
	}

	var problems []string
	err := repositorycheck.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			if owner, banned := owners[identifier.Name]; banned {
				line := fileSet.Position(identifier.Pos()).Line
				problems = append(problems, fmt.Sprintf("%s:%d uses %s, forbidden by term %s", filepath.ToSlash(relative), line, identifier.Name, owner))
			}
			return true
		})
		return nil
	})
	return problems, err
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
