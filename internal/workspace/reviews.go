package workspace

import (
	"errors"
	"path"
	"regexp"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// SecretReview records an operator-reviewed false positive without retaining
// a matched value. Any source edit invalidates the review, including replacement
// by a real credential at the same offsets.
type SecretReview struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Fingerprint string `json:"fingerprint"`
	Reason      string `json:"reason"`
}

var findingFingerprintPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

func validateSecretReviews(reviews []SecretReview) error {
	if len(reviews) > 512 {
		return errors.New("workspace secret reviews exceed the review limit")
	}
	seen := map[string]bool{}
	for _, review := range reviews {
		if !safeText(review.Path, 1024) || strings.ContainsAny(review.Path, `\:`) || strings.HasPrefix(review.Path, "/") || path.Clean(review.Path) != review.Path || review.Path == "." || review.Path == ".." || strings.HasPrefix(review.Path, "../") || !digestPattern.MatchString(review.SHA256) || !findingFingerprintPattern.MatchString(review.Fingerprint) {
			return errors.New("secret reviews require a canonical relative path, complete source SHA-256 and finding fingerprint")
		}
		switch review.Reason {
		case "test_fixture", "documented_placeholder", "source_reference":
		default:
			return errors.New("secret reviews must identify a reviewed false positive")
		}
		key := review.Path + "\x00" + review.Fingerprint
		if seen[key] {
			return errors.New("duplicate workspace secret review")
		}
		seen[key] = true
	}
	return nil
}

// LoadSecretReviews reads an explicit private operator policy. A file in the
// source cannot authorize itself by containing similarly named review records.
func LoadSecretReviews(filename string) ([]SecretReview, error) {
	if err := rejectSymlinks(filename); err != nil {
		return nil, err
	}
	data, err := readPrivateFile(filename, maximumRegistryBytes)
	if err != nil {
		return nil, err
	}
	var reviews []SecretReview
	if err := strictJSON(data, &reviews); err != nil {
		return nil, err
	}
	if err := validateSecretReviews(reviews); err != nil {
		return nil, err
	}
	return reviews, nil
}

// CountReviewedSecrets counts exact high-confidence false-positive reviews.
// Findings, coverage and redaction remain intact. Changed files and unmatched
// reviews never reduce the unreviewed finding count.
func CountReviewedSecrets(bundle rkcmodel.Bundle, reviews []SecretReview) int {
	approved := map[string]SecretReview{}
	for _, review := range reviews {
		approved[review.Path+"\x00"+review.Fingerprint] = review
	}
	artifacts := map[string]rkcmodel.Artifact{}
	for _, artifact := range bundle.Artifacts {
		artifacts[artifact.ID] = artifact
	}
	count := 0
	counted := map[string]bool{}
	for _, node := range bundle.Nodes {
		if !rkcmodel.IsHighConfidenceSecret(node) || node.Source == nil {
			continue
		}
		fingerprint, _ := node.Attributes["fingerprint"].(string)
		key := node.Source.Path + "\x00" + fingerprint
		review, ok := approved[key]
		artifact, present := artifacts[node.ArtifactID]
		if ok && present && !counted[key] && node.Source.ArtifactID == artifact.ID && artifact.Path == review.Path && artifact.SHA256 == review.SHA256 {
			count++
			counted[key] = true
		}
	}
	return count
}
