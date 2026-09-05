package githubsource

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

// Materialize resolves the selected default branch once, then downloads its
// immutable commit archive. The caller supplies a private, existing empty
// directory. Only a complete verified extraction is published under its name.
// It is a source snapshot, not a Git checkout; archive bytes may be recompressed
// by GitHub, so both the commit and the received archive digest are retained.
func (client *Client) Materialize(ctx context.Context, fullName, directory string) (Checkout, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	root, absolute, err := openDestination(directory)
	if err != nil {
		return Checkout{}, err
	}
	defer root.Close()
	repository, err := client.Repository(ctx, fullName)
	if err != nil {
		return Checkout{}, err
	}
	if repository.DefaultBranch == "" {
		return Checkout{}, errors.New("this repository has no default branch to download")
	}
	publishedName := strings.Split(repository.FullName, "/")[1]
	if _, err := archivePath("source/"+publishedName, false); err != nil {
		return Checkout{}, errors.New("repository name is not supported as a portable folder name")
	}
	var revision struct {
		SHA string `json:"sha"`
	}
	if err := client.getJSON(ctx, "/repos/"+repository.FullName+"/commits/"+url.PathEscape(repository.DefaultBranch), &revision); err != nil {
		return Checkout{}, err
	}
	if !commitID.MatchString(revision.SHA) {
		return Checkout{}, errors.New("GitHub did not resolve an immutable commit")
	}
	revision.SHA = strings.ToLower(revision.SHA)
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return Checkout{}, errors.New("create GitHub staging identity")
	}
	stage := ".rkc-github-" + hex.EncodeToString(random)
	if err := root.Mkdir(stage, 0o700); err != nil {
		return Checkout{}, errors.New("create GitHub staging directory")
	}
	defer root.RemoveAll(stage)
	archive, err := root.OpenFile(stage+"/archive.zip", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return Checkout{}, errors.New("create GitHub archive file")
	}
	defer archive.Close()
	response, err := client.get(ctx, "https://api.github.com/repos/"+repository.FullName+"/zipball/"+revision.SHA)
	if err != nil {
		return Checkout{}, err
	}
	digest := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(archive, digest), io.LimitReader(&contextReader{ctx: ctx, reader: response.Body}, client.limits.compressed+1))
	response.Body.Close()
	if copyErr != nil {
		return Checkout{}, requestError(ctx, "download GitHub archive")
	}
	if count > client.limits.compressed {
		return Checkout{}, errors.New("GitHub archive exceeds the compressed byte limit")
	}
	if err := root.Mkdir(stage+"/repository", 0o700); err != nil {
		return Checkout{}, errors.New("create GitHub extraction directory")
	}
	payload, err := root.OpenRoot(stage + "/repository")
	if err != nil {
		return Checkout{}, errors.New("open GitHub extraction directory")
	}
	extractErr := client.extract(ctx, archive, count, payload)
	payload.Close()
	// Close archive handles before staging cleanup or rename on Windows.
	archive.Close()
	if extractErr != nil {
		return Checkout{}, extractErr
	}
	if err := ctx.Err(); err != nil {
		return Checkout{}, err
	}
	entries, err := readRoot(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != stage {
		return Checkout{}, errors.New("GitHub destination changed during download")
	}
	if err := publishNoReplace(root, stage+"/repository", publishedName); err != nil {
		return Checkout{}, errors.New("publish GitHub source snapshot")
	}
	return Checkout{
		Root: filepath.Join(absolute, publishedName), Repository: repository, CommitSHA: revision.SHA,
		ArchiveSHA256: hex.EncodeToString(digest.Sum(nil)), ArchiveBytes: count,
	}, nil
}

func openDestination(directory string) (*os.Root, string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, "", errors.New("resolve GitHub destination")
	}
	info, err := privatepath.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(absolute, info) {
		return nil, "", errors.New("GitHub destination must be an existing owner directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, "", errors.New("GitHub destination must be private to its owner")
	}
	// Canonicalize platform-owned parent aliases (for example macOS /var),
	// while rejecting a symbolic link at the caller-supplied destination itself.
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, "", errors.New("resolve GitHub destination links")
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, "", errors.New("open GitHub destination")
	}
	entries, err := readRoot(root)
	if err != nil || len(entries) != 0 {
		root.Close()
		return nil, "", errors.New("GitHub destination must be empty")
	}
	return root, absolute, nil
}

func readRoot(root *os.Root) ([]os.DirEntry, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(2)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return entries, err
}

type archiveEntry struct {
	file *zip.File
	path string
}

type pathRecord struct {
	spelling  string
	directory bool
	explicit  bool
}

func (client *Client) extract(ctx context.Context, file *os.File, bytes int64, destination *os.Root) error {
	if err := preflightZIP(ctx, file, bytes, client.limits.paths); err != nil {
		return err
	}
	reader, err := zip.NewReader(file, bytes)
	if err != nil {
		return errors.New("GitHub archive is not a valid ZIP file")
	}
	if len(reader.File) == 0 || len(reader.File) > client.limits.paths {
		return errors.New("GitHub archive path count is outside the supported limit")
	}
	paths := map[string]pathRecord{}
	entries := make([]archiveEntry, 0, len(reader.File))
	prefix := ""
	var expanded uint64
	files := 0
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.Flags&1 != 0 || (file.Method != zip.Store && file.Method != zip.Deflate) {
			return errors.New("GitHub archive uses an unsupported ZIP encoding")
		}
		directory := file.FileInfo().IsDir()
		if !directory && !file.Mode().IsRegular() || directory && file.Mode()&os.ModeType != os.ModeDir {
			return errors.New("GitHub archives containing symbolic links or special files are not supported")
		}
		parts, err := archivePath(file.Name, directory)
		if err != nil {
			return err
		}
		if prefix == "" {
			prefix = parts[0]
		}
		if parts[0] != prefix || (len(parts) == 1 && !directory) {
			return errors.New("GitHub archive must contain one repository root directory")
		}
		if len(parts) == 1 {
			continue
		}
		parts = parts[1:]
		for i := range parts {
			path := strings.Join(parts[:i+1], "/")
			key := foldPath(path)
			isDirectory := i < len(parts)-1 || directory
			explicit := i == len(parts)-1
			if prior, exists := paths[key]; exists {
				if prior.spelling != path || prior.directory != isDirectory || explicit && prior.explicit {
					return errors.New("GitHub archive contains duplicate or colliding paths")
				}
				explicit = explicit || prior.explicit
			}
			paths[key] = pathRecord{path, isDirectory, explicit}
			if len(paths) > client.limits.paths {
				return errors.New("GitHub archive exceeds the expanded path limit")
			}
		}
		if file.UncompressedSize64 > uint64(client.limits.file) || file.UncompressedSize64 > uint64(client.limits.expanded)-expanded {
			return errors.New("GitHub archive exceeds the expanded byte limit")
		}
		if directory && file.UncompressedSize64 != 0 {
			return errors.New("GitHub archive directory contains unexpected data")
		}
		expanded += file.UncompressedSize64
		if !directory {
			files++
		}
		entries = append(entries, archiveEntry{file, strings.Join(parts, "/")})
	}
	if files == 0 {
		return errors.New("GitHub archive contains no regular source files")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.file.FileInfo().IsDir() {
			if err := destination.MkdirAll(entry.path, 0o700); err != nil {
				return errors.New("create GitHub source directory")
			}
			continue
		}
		if err := destination.MkdirAll(filepath.Dir(entry.path), 0o700); err != nil {
			return errors.New("create GitHub source parent directory")
		}
		input, err := entry.file.Open()
		if err != nil {
			return errors.New("open GitHub source archive entry")
		}
		output, err := destination.OpenFile(entry.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			input.Close()
			return errors.New("create GitHub source file; a filesystem path may collide")
		}
		written, copyErr := io.Copy(output, io.LimitReader(&contextReader{ctx: ctx, reader: input}, client.limits.file+1))
		input.Close()
		closeErr := output.Close()
		if copyErr != nil || closeErr != nil || written != int64(entry.file.UncompressedSize64) {
			return requestError(ctx, "GitHub archive content failed size or integrity verification")
		}
	}
	return ctx.Err()
}

// Check the bounded central-directory declaration before archive/zip allocates
// a File for every record. ZIP64 is unnecessary within this client's limits and
// is rejected explicitly instead of accepting a second, wider record count.
func preflightZIP(ctx context.Context, file *os.File, size int64, maximumPaths int) error {
	length := min(size, int64(22+65535+20))
	if length < 22 {
		return errors.New("GitHub archive has no ZIP directory")
	}
	tail := make([]byte, int(length))
	if _, err := file.ReadAt(tail, size-length); err != nil {
		return errors.New("read GitHub archive directory")
	}
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:i+4]) != 0x06054b50 || i+22+int(binary.LittleEndian.Uint16(tail[i+20:i+22])) != len(tail) {
			continue
		}
		count := binary.LittleEndian.Uint16(tail[i+10 : i+12])
		centralBytes := binary.LittleEndian.Uint32(tail[i+12 : i+16])
		offset := binary.LittleEndian.Uint32(tail[i+16 : i+20])
		if binary.LittleEndian.Uint16(tail[i+4:i+6]) != 0 || binary.LittleEndian.Uint16(tail[i+6:i+8]) != 0 ||
			binary.LittleEndian.Uint16(tail[i+8:i+10]) != count || count == 0xffff || count == 0 || int(count) > maximumPaths ||
			centralBytes > 64<<20 || uint64(offset)+uint64(centralBytes) > uint64(size-length+int64(i)) ||
			i >= 20 && binary.LittleEndian.Uint32(tail[i-20:i-16]) == 0x07064b50 {
			return errors.New("GitHub archive exceeds supported ZIP directory bounds")
		}
		// ZIP readers may use the actual central-directory record count even
		// when the end record understates it. Validate every record up front.
		var records int
		position, end := int64(offset), int64(offset)+int64(centralBytes)
		header := make([]byte, 46)
		for position < end {
			if err := ctx.Err(); err != nil {
				return err
			}
			if end-position < 46 {
				return errors.New("GitHub ZIP directory is truncated")
			}
			if _, err := file.ReadAt(header, position); err != nil || binary.LittleEndian.Uint32(header[:4]) != 0x02014b50 {
				return errors.New("GitHub ZIP directory is invalid")
			}
			nameBytes := int64(binary.LittleEndian.Uint16(header[28:30]))
			if nameBytes == 0 || nameBytes > 4096 {
				return errors.New("GitHub ZIP filename exceeds the supported limit")
			}
			position += 46 + nameBytes + int64(binary.LittleEndian.Uint16(header[30:32])) + int64(binary.LittleEndian.Uint16(header[32:34]))
			records++
			if records > maximumPaths || position > end {
				return errors.New("GitHub ZIP directory exceeds the path limit")
			}
		}
		if records != int(count) {
			return errors.New("GitHub ZIP directory record counts disagree")
		}
		return nil
	}
	return errors.New("GitHub archive has no valid ZIP directory")
}

func archivePath(name string, directory bool) ([]string, error) {
	if directory {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || len(name) > 4096 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\:<>\"|?*") {
		return nil, errors.New("GitHub archive contains an unsupported portable path")
	}
	parts := strings.Split(name, "/")
	if len(parts) > 128 {
		return nil, errors.New("GitHub archive path nesting exceeds the limit")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 255 || strings.TrimRight(part, ". ") != part || strings.EqualFold(part, ".git") {
			return nil, errors.New("GitHub archive contains an unsafe path component")
		}
		for _, r := range part {
			if unicode.IsControl(r) {
				return nil, errors.New("GitHub archive path contains control characters")
			}
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$" ||
			len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' ||
			(strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && strings.ContainsAny(strings.TrimPrefix(strings.TrimPrefix(base, "COM"), "LPT"), "¹²³") {
			return nil, errors.New("GitHub archive path uses a reserved Windows name")
		}
	}
	return parts, nil
}

func foldPath(path string) string {
	return strings.Map(func(r rune) rune {
		minimum := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		return minimum
	}, path)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
