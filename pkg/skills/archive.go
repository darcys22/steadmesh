package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/darcys22/steadmesh/pkg/spec"
)

func (c *Catalog) download(ctx context.Context, source spec.SkillMarketplace) (*cachedRepository, error) {
	// Source validation is repeated here so this lower-level operation cannot
	// accidentally become a general-purpose HTTP fetcher.
	if err := validateSource("source", source); err != nil {
		return nil, err
	}
	url := "https://codeload.github.com/" + source.Repository + "/zip/" + strings.ToLower(source.Ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/zip")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "steadmesh-skill-catalog")
	response, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch marketplace archive: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch marketplace archive: HTTP %d", response.StatusCode)
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, fmt.Errorf("unexpected archive content encoding %q", encoding)
	}
	if response.ContentLength > maxArchiveBytes {
		return nil, fmt.Errorf("marketplace archive exceeds %d compressed bytes", maxArchiveBytes)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read marketplace archive: %w", err)
	}
	if len(data) > maxArchiveBytes {
		return nil, fmt.Errorf("marketplace archive exceeds %d compressed bytes", maxArchiveBytes)
	}
	return readArchive(ctx, data)
}

// readArchive never writes to the filesystem. Validate every member, including
// files outside selected skills, before trusting any manifest or file content.
func readArchive(ctx context.Context, data []byte) (*cachedRepository, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid marketplace ZIP archive: %w", err)
	}
	if len(archive.File) == 0 || len(archive.File) > maxArchiveFiles {
		return nil, fmt.Errorf("marketplace archive must contain 1-%d entries", maxArchiveFiles)
	}
	root := ""
	seen := make(map[string]bool, len(archive.File))
	files := make(map[string][]byte)
	total := 0
	for _, file := range archive.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(file.Name, "/")
		if err := safeArchivePath(name); err != nil {
			return nil, fmt.Errorf("unsafe archive entry %q: %w", file.Name, err)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate archive entry %q", name)
		}
		mode := file.Mode()
		isDir := file.FileInfo().IsDir()
		if mode.Type() != 0 && mode.Type() != fs.ModeDir {
			return nil, fmt.Errorf("archive entry %q is not a regular file or directory", name)
		}
		if file.Flags&1 != 0 {
			return nil, fmt.Errorf("encrypted archive entry %q is unsupported", name)
		}
		if file.Method != zip.Store && file.Method != zip.Deflate {
			return nil, fmt.Errorf("unsupported ZIP method for %q", name)
		}
		seen[name] = isDir
		parts := strings.SplitN(name, "/", 2)
		if root == "" {
			root = parts[0]
		} else if root != parts[0] {
			return nil, fmt.Errorf("marketplace archive must have a single repository root")
		}
		if len(parts) == 1 {
			if !isDir {
				return nil, fmt.Errorf("marketplace archive entry has no repository root directory")
			}
			continue
		}
		if isDir {
			if file.UncompressedSize64 != 0 {
				return nil, fmt.Errorf("archive directory %q contains data", name)
			}
			continue
		}
		if file.UncompressedSize64 > maxFileBytes {
			return nil, fmt.Errorf("archive file %q exceeds %d bytes", name, maxFileBytes)
		}
		if uint64(total)+file.UncompressedSize64 > maxExpandedBytes {
			return nil, fmt.Errorf("archive exceeds %d expanded bytes", maxExpandedBytes)
		}
		reader, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open archive file %q: %w", name, err)
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, maxFileBytes+1))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read archive file %q: %w", name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close archive file %q: %w", name, closeErr)
		}
		if len(content) > maxFileBytes || total+len(content) > maxExpandedBytes {
			return nil, fmt.Errorf("marketplace archive exceeds expanded size limits")
		}
		total += len(content)
		files[parts[1]] = content
	}
	// Reject file/directory collisions even when ZIP entries are reordered or
	// an intermediate directory has no explicit directory entry.
	for name := range seen {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if dir, exists := seen[parent]; exists && !dir {
				return nil, fmt.Errorf("archive file/directory collision at %q", parent)
			}
		}
	}
	return &cachedRepository{files: files, size: total}, nil
}

func safeArchivePath(name string) error {
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\:") || strings.HasPrefix(name, "/") {
		return fmt.Errorf("invalid relative path")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("control character in path")
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("non-canonical or traversing path")
		}
	}
	return nil
}
