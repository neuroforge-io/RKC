package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

const maximumRegistryBytes = 1 << 20

var aliasPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var generationPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}-[a-f0-9]{32}$`)

// Load reads one bounded, strict, owner-private registry. It never repairs a
// path or falls back to a previously observed authorization roster.
func Load(path string) (Registry, error) {
	var registry Registry
	path, err := filepath.Abs(path)
	if err != nil {
		return registry, err
	}
	if err := rejectSymlinks(path); err != nil {
		return registry, err
	}
	root := filepath.Dir(path)
	identity, err := privatepath.Lstat(root)
	if err != nil {
		return registry, err
	}
	if err := privatepath.CheckDir(root, identity); err != nil {
		return registry, err
	}
	data, err := readPrivateFile(path, maximumRegistryBytes)
	if err != nil {
		return registry, err
	}
	if err := strictJSON(data, &registry); err != nil {
		return registry, fmt.Errorf("invalid workspace registry: %w", err)
	}
	if err := validate(registry, root); err != nil {
		return registry, err
	}
	if err := privatepath.CheckDir(root, identity); err != nil {
		return Registry{}, err
	}
	return registry, nil
}

func readPrivateFile(path string, maximum int64) ([]byte, error) {
	before, err := privatepath.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := privatepath.CheckFile(path, before); err != nil {
		return nil, err
	}
	if before.Size() > maximum {
		return nil, errors.New("workspace file exceeds its size bound")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("workspace file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || len(data) > int(maximum) || int64(len(data)) != before.Size() || after.Size() != before.Size() || after.ModTime() != before.ModTime() {
		return nil, errors.New("workspace file changed while reading")
	}
	// Atomic replacement by the writer is allowed: the opened old generation
	// remains a complete read. In-place mutation fails the state checks above.
	return data, nil
}

func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var consume func(int) error
	consume = func(depth int) error {
		if depth > 16 {
			return errors.New("workspace JSON nesting limit exceeded")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || key != strings.ToLower(key) || seen[key] {
					return errors.New("duplicate or noncanonical workspace JSON field")
				}
				seen[key] = true
				if err := consume(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := consume(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid workspace JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := consume(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing workspace JSON")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func validate(registry Registry, root string) error {
	if registry.SchemaVersion != SchemaVersion || registry.Generation == 0 || registry.Sources == nil || len(registry.Sources) > MaximumSources {
		return errors.New("unsupported or unbounded workspace registry")
	}
	seen := map[string]bool{}
	for _, source := range registry.Sources {
		if err := validateSource(source); err != nil {
			return err
		}
		if seen[source.ID] {
			return errors.New("duplicate workspace source alias")
		}
		seen[source.ID] = true
		for _, active := range []*Active{source.Active, source.Previous} {
			if active == nil {
				continue
			}
			if active.ReviewedSecretFindings < 0 || active.ReviewedSecretFindings > 512 {
				return errors.New("invalid reviewed finding count")
			}
			if !generationPattern.MatchString(active.Generation) || !strings.HasPrefix(active.Generation, source.ID+"-") ||
				active.AtlasPath != filepath.Join(root, "generations", active.Generation, "atlas") ||
				!safeText(active.SnapshotID, 256) || !digestPattern.MatchString(active.ManifestSHA256) || !digestPattern.MatchString(active.Fingerprint) || !safeText(active.CompilerVersion, 128) {
				return errors.New("invalid workspace active generation")
			}
		}
		if source.Active == nil && (source.Previous != nil || source.Freshness.Status == "current") {
			return errors.New("workspace current status requires an active generation")
		}
		switch source.Freshness.Status {
		case "pending", "current", "stale", "error":
		default:
			return errors.New("unknown workspace freshness status")
		}
		switch source.Freshness.ErrorCode {
		case "", "refresh_failed", "source_changed", "canceled", "source_unavailable", "quality_failed":
		default:
			return errors.New("unknown workspace error code")
		}
	}
	return nil
}

func validateSource(source Source) error {
	if err := validateSecretReviews(source.SecretReviews); err != nil {
		return err
	}
	if !aliasPattern.MatchString(source.ID) || !safeText(source.Label, 160) {
		return errors.New("workspace alias must be lowercase path-safe text and label must be bounded text")
	}
	if source.Kind == "local" {
		if !filepath.IsAbs(source.LocalPath) || filepath.Clean(source.LocalPath) != source.LocalPath || source.RemoteURL != "" || source.Ref != "" {
			return errors.New("local workspace source requires one absolute folder")
		}
	} else if source.Kind == "git" {
		if source.LocalPath != "" || validateRemote(source.RemoteURL, source.Ref) != nil {
			return errors.New("remote workspace source requires a credential-free HTTPS or SSH Git URL")
		}
	} else {
		return errors.New("unknown workspace source kind")
	}
	limits := source.Limits
	if limits.MaxFiles < 1 || limits.MaxFiles > 500000 || limits.MaxFileBytes < 1 || limits.MaxFileBytes > 1<<30 || limits.MaxRepositoryBytes < 1 || limits.MaxRepositoryBytes > 20<<30 || limits.MaxTextBytes < 1 || limits.MaxTextBytes > 8<<20 || limits.MaxTextBytes > limits.MaxFileBytes {
		return errors.New("workspace inventory limits must be positive and within product ceilings")
	}
	if source.Excludes == nil || len(source.Excludes) > 512 {
		return errors.New("workspace exclusions must be a bounded list")
	}
	if err := ValidateExcludePatterns(source.ExcludePatterns); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, exclude := range source.Excludes {
		if !safeText(exclude, 1024) || strings.Contains(exclude, `\`) || strings.Contains(exclude, ":") || strings.HasPrefix(exclude, "/") || filepath.ToSlash(filepath.Clean(exclude)) != exclude || exclude == "." || exclude == ".." || strings.HasPrefix(exclude, "../") || seen[exclude] {
			return errors.New("workspace exclusions must be unique repository-relative paths")
		}
		seen[exclude] = true
	}
	return nil
}

func validateRemote(raw, ref string) error {
	u, err := url.Parse(raw)
	if err != nil || !safeText(raw, 2048) || u.Host == "" || u.Path == "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "ssh") {
		return errors.New("invalid remote URL")
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword || u.Scheme != "ssh" || u.User.Username() != "git" {
			return errors.New("remote URL credentials are not accepted")
		}
	}
	if ref != "" && (!safeText(ref, 256) || strings.HasPrefix(ref, "-")) {
		return errors.New("invalid remote reference")
	}
	return nil
}

func safeText(value string, limit int) bool {
	return value != "" && len(value) <= limit && value == strings.TrimSpace(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

func rejectSymlinks(path string) error {
	for {
		info, err := privatepath.Lstat(path)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("workspace paths must not contain symlinks")
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}
