package capture

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Readable archive layout. Every stored body also gets a human path under
// files/<host>/<path> so a person can find it in Finder by URL; the streamer's
// own idea of the path is never trusted. Derivation rules live in
// CONTRACT2-layout.md and are recomputed here from URL, mime and sha256.
const (
	filesDirName = "files"
	indexName    = "index.txt"

	maxHostLen   = 80
	maxSegLen    = 80
	maxPathDepth = 8
	maxQueryLen  = 40
)

var (
	unsafeFileChar  = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	queryRunPattern = regexp.MustCompile(`[^A-Za-z0-9]+`)
	fileExtPattern  = regexp.MustCompile(`^\.[A-Za-z0-9]{1,5}$`)
)

// deriveRel builds the archive-relative path (`files/<host>/<segments>`) for one
// item. It only composes sanitized atoms, so the result cannot escape the
// archive; anything that tries is rejected with ErrInvalidPath.
func deriveRel(rawURL, finalURL, mime string) (string, error) {
	src := finalURL
	if src == "" {
		src = rawURL
	}
	var u *url.URL
	if src != "" {
		parsed, err := url.Parse(src)
		if err != nil {
			return "", ErrInvalidPath
		}
		u = parsed
	}

	segs, trailing, err := splitPathSegments(u)
	if err != nil {
		return "", err
	}
	name := "index"
	var dirs []string
	if len(segs) > 0 && !trailing {
		dirs = segs[:len(segs)-1]
		name = segs[len(segs)-1]
	} else {
		dirs = segs
	}

	ext := ""
	stem := name
	if own := path.Ext(name); fileExtPattern.MatchString(own) {
		ext = strings.ToLower(own)
		stem = strings.TrimSuffix(name, own)
	} else if mapped := mimeExtension(mime); mapped != "" {
		ext = mapped
	}
	if q := queryToken(u); q != "" {
		stem += "__" + q
	}

	parts := make([]string, 0, len(dirs)+3)
	parts = append(parts, filesDirName, deriveHost(u))
	parts = append(parts, dirs...)
	parts = append(parts, stem+ext)
	return strings.Join(parts, "/"), nil
}

// deriveHost lowercases the hostname and appends a non-default port.
func deriveHost(u *url.URL) string {
	host := ""
	if u != nil {
		host = strings.ToLower(u.Hostname())
		if port := u.Port(); port != "" && !defaultPort(u.Scheme, port) {
			host += "__" + port
		}
	}
	host = strings.ToLower(unsafeFileChar.ReplaceAllString(host, "_"))
	host = strings.Trim(host, "._-")
	if host == "" {
		host = "local"
	}
	if len(host) > maxHostLen {
		host = strings.TrimRight(host[:maxHostLen], "._-")
	}
	if host == "" {
		host = "local"
	}
	return host
}

func defaultPort(scheme, port string) bool {
	switch strings.ToLower(scheme) {
	case "http":
		return port == "80"
	case "https":
		return port == "443"
	}
	return false
}

// splitPathSegments percent-decodes and sanitizes the URL path. It reports
// whether the path ends in "/" (which makes the last component `index`). `..`,
// `.`, backslashes and NUL bytes are rejected outright rather than rewritten.
func splitPathSegments(u *url.URL) ([]string, bool, error) {
	if u == nil {
		return nil, true, nil
	}
	escaped := u.EscapedPath()
	if escaped == "" {
		escaped = u.Path
	}
	if escaped == "" {
		return nil, true, nil
	}
	trailing := strings.HasSuffix(escaped, "/")
	parts := strings.Split(strings.Trim(escaped, "/"), "/")
	segs := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		decoded, err := url.PathUnescape(part)
		if err != nil {
			decoded = part
		}
		if strings.ContainsAny(decoded, "\\\x00/") || decoded == "." || decoded == ".." {
			return nil, false, ErrInvalidPath
		}
		seg := sanitizeSegment(decoded)
		if seg == "" || seg == "." || seg == ".." {
			return nil, false, ErrInvalidPath
		}
		segs = append(segs, seg)
	}
	if len(segs) > maxPathDepth {
		head := append([]string(nil), segs[:maxPathDepth-1]...)
		segs = append(head, segs[len(segs)-1])
	}
	return segs, trailing, nil
}

func sanitizeSegment(s string) string {
	s = unsafeFileChar.ReplaceAllString(s, "_")
	if len(s) > maxSegLen {
		s = s[:maxSegLen]
	}
	return s
}

// queryToken collapses the query string into a filename-safe token.
func queryToken(u *url.URL) string {
	if u == nil || u.RawQuery == "" {
		return ""
	}
	q := strings.Trim(queryRunPattern.ReplaceAllString(u.RawQuery, "_"), "_")
	if len(q) > maxQueryLen {
		q = strings.TrimRight(q[:maxQueryLen], "_")
	}
	return q
}

// mimeExtension maps a normalized mime type to the extension appended when the
// URL path carries no usable one.
func mimeExtension(mime string) string {
	m := normalizeMime(mime)
	switch {
	case m == "text/html":
		return ".html"
	case m == "text/css":
		return ".css"
	case strings.Contains(m, "javascript") || strings.Contains(m, "ecmascript"):
		return ".js"
	case m == "application/json" || strings.HasSuffix(m, "+json"):
		return ".json"
	case m == "image/svg+xml":
		return ".svg"
	case m == "text/xml" || m == "application/xml" || strings.HasSuffix(m, "+xml"):
		return ".xml"
	case m == "image/png":
		return ".png"
	case m == "image/jpeg":
		return ".jpg"
	case m == "image/gif":
		return ".gif"
	case m == "image/webp":
		return ".webp"
	case m == "image/avif":
		return ".avif"
	case m == "font/woff2":
		return ".woff2"
	case m == "font/woff":
		return ".woff"
	case m == "font/ttf":
		return ".ttf"
	case strings.Contains(m, "wasm"):
		return ".wasm"
	case m == "text/plain":
		return ".txt"
	}
	return ""
}

// versionedRel inserts `~<tag>` before the last extension.
func versionedRel(rel, tag string) string {
	dir := path.Dir(rel)
	base := path.Base(rel)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return path.Join(dir, stem+"~"+tag+ext)
}

// resolveRel applies the collision rules against the files already recorded in
// this archive: the same path and sha reuses the path, a different sha gets a
// version suffix, and a second collision falls back to the full hash.
func (rec *recording) resolveRel(base, sha string) string {
	return resolvePath(rec.paths, base, sha)
}

// resolvePath applies the collision rules to a path map shared by a recording or
// a migration.
func resolvePath(paths map[string]string, base, sha string) string {
	if existing, ok := paths[base]; ok {
		if existing == sha {
			return base
		}
		short := versionedRel(base, sha[:12])
		if prev, ok := paths[short]; ok {
			if prev == sha {
				return short
			}
			return versionedRel(base, sha)
		}
		return short
	}
	return base
}

// validFileRel accepts only `files/<component>(/<component>)*` with plain
// components, so a route parameter can never reach outside the archive.
func validFileRel(rel string) (string, error) {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsAny(rel, "\\\x00") {
		return "", ErrInvalidPath
	}
	if path.Clean(rel) != rel {
		return "", ErrInvalidPath
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || parts[0] != filesDirName {
		return "", ErrInvalidPath
	}
	for _, part := range parts[1:] {
		if part == "" || part == "." || part == ".." {
			return "", ErrInvalidPath
		}
	}
	return rel, nil
}

// renderIndex produces the human index: one tab-separated line per item,
// sorted by URL then request time.
func renderIndex(items []Item) []byte {
	rows := append([]Item(nil), items...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].URL != rows[j].URL {
			return rows[i].URL < rows[j].URL
		}
		return rows[i].TSRequested < rows[j].TSRequested
	})
	var b strings.Builder
	for _, it := range rows {
		file := it.File
		if file == "" {
			file = "-"
		}
		sha := it.SHA256
		if len(sha) > 12 {
			sha = sha[:12]
		}
		b.WriteString(indexField(it.URL))
		b.WriteByte('\t')
		b.WriteString(indexField(file))
		b.WriteByte('\t')
		b.WriteString(indexField(it.State))
		b.WriteByte('\t')
		b.WriteString(indexField(it.Mime))
		b.WriteByte('\t')
		b.WriteString(strconv.FormatInt(it.SizeDecoded, 10))
		b.WriteByte('\t')
		b.WriteString(indexField(sha))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func indexField(s string) string {
	if s == "" {
		return "-"
	}
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}

// writeIndex rewrites index.txt atomically (tmp + rename in the archive dir).
func writeIndex(dir string, items []Item) error {
	data := renderIndex(items)
	tmp, err := os.CreateTemp(dir, indexName+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, indexName))
}
