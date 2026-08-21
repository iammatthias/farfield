package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
)

// EpubMeta is the bibliographic metadata extracted from an EPUB's OPF package.
type EpubMeta struct {
	Title       string
	Author      string
	Language    string
	Identifier  string
	Description string
}

// containerXML is META-INF/container.xml — it points at the OPF package file.
type containerXML struct {
	Rootfiles []struct {
		FullPath  string `xml:"full-path,attr"`
		MediaType string `xml:"media-type,attr"`
	} `xml:"rootfiles>rootfile"`
}

// opfPackage is the EPUB OPF package document: Dublin Core metadata plus the
// manifest used to locate the cover image. Element tags use bare local names
// (matching any namespace) for OPF elements, and the fixed Dublin Core
// namespace URI for the dc:* metadata fields.
type opfPackage struct {
	XMLName  xml.Name `xml:"package"`
	Metadata struct {
		Title       []string `xml:"http://purl.org/dc/elements/1.1/ title"`
		Creator     []string `xml:"http://purl.org/dc/elements/1.1/ creator"`
		Language    []string `xml:"http://purl.org/dc/elements/1.1/ language"`
		Identifier  []string `xml:"http://purl.org/dc/elements/1.1/ identifier"`
		Description []string `xml:"http://purl.org/dc/elements/1.1/ description"`
		Metas       []struct {
			Name    string `xml:"name,attr"`
			Content string `xml:"content,attr"`
		} `xml:"meta"`
	} `xml:"metadata"`
	Manifest struct {
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			MediaType  string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
}

// parseEPUB reads bibliographic metadata and the cover image out of EPUB
// bytes already in memory. Prefer parseEPUBAt for uploads — a book on disk
// never needs to be buffered to be read.
func parseEPUB(data []byte) (meta EpubMeta, coverBytes []byte, coverMime string, err error) {
	return parseEPUBAt(bytes.NewReader(data), int64(len(data)))
}

// parseEPUBAt reads bibliographic metadata and the cover image out of an EPUB
// through random access, so the archive is never held in memory — only the
// handful of small entries it actually reads (container.xml, the OPF, the
// cover image). It returns an error when the source is not a recognisable
// EPUB (no zip, no container.xml, or no OPF package) — that error is the
// upload validation. A missing or unreadable cover is not an error:
// coverBytes is simply nil.
func parseEPUBAt(ra io.ReaderAt, size int64) (meta EpubMeta, coverBytes []byte, coverMime string, err error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return meta, nil, "", fmt.Errorf("not a zip archive: %w", err)
	}

	containerBytes, err := readZip(zr, "META-INF/container.xml")
	if err != nil {
		return meta, nil, "", fmt.Errorf("missing META-INF/container.xml — not an EPUB")
	}
	var container containerXML
	if err := xml.Unmarshal(containerBytes, &container); err != nil || len(container.Rootfiles) == 0 {
		return meta, nil, "", fmt.Errorf("unreadable container.xml")
	}
	opfPath := strings.TrimSpace(container.Rootfiles[0].FullPath)
	if opfPath == "" {
		return meta, nil, "", fmt.Errorf("no OPF rootfile declared")
	}

	opfBytes, err := readZip(zr, opfPath)
	if err != nil {
		return meta, nil, "", fmt.Errorf("missing OPF package %q", opfPath)
	}
	var pkg opfPackage
	if err := xml.Unmarshal(opfBytes, &pkg); err != nil {
		return meta, nil, "", fmt.Errorf("unreadable OPF package: %w", err)
	}

	meta = EpubMeta{
		Title:       firstNonEmpty(pkg.Metadata.Title),
		Author:      firstNonEmpty(pkg.Metadata.Creator),
		Language:    firstNonEmpty(pkg.Metadata.Language),
		Identifier:  firstNonEmpty(pkg.Metadata.Identifier),
		Description: firstNonEmpty(pkg.Metadata.Description),
	}

	if href, mime := findCover(pkg); href != "" {
		if unescaped, err := url.PathUnescape(href); err == nil {
			href = unescaped
		}
		entry := path.Join(path.Dir(opfPath), href)
		if cb, err := readZip(zr, entry); err == nil && len(cb) > 0 {
			coverBytes, coverMime = cb, mime
		}
	}
	return meta, coverBytes, coverMime, nil
}

// findCover locates the cover image in the OPF manifest: first an EPUB 3 item
// flagged properties="cover-image", then the EPUB 2 fallback of a
// <meta name="cover" content="<id>"/> pointing at a manifest item.
func findCover(pkg opfPackage) (href, mime string) {
	for _, it := range pkg.Manifest.Items {
		if strings.Contains(it.Properties, "cover-image") {
			return it.Href, it.MediaType
		}
	}
	var coverID string
	for _, m := range pkg.Metadata.Metas {
		if strings.EqualFold(strings.TrimSpace(m.Name), "cover") {
			coverID = m.Content
			break
		}
	}
	if coverID != "" {
		for _, it := range pkg.Manifest.Items {
			if it.ID == coverID {
				return it.Href, it.MediaType
			}
		}
	}
	return "", ""
}

// maxZipEntry caps how much of a single zip entry epub parsing will decompress.
//
// The entries read here are metadata — container.xml, the OPF package
// document, a cover image — and none is legitimately large. Without a cap,
// io.ReadAll expanded whatever the archive claimed: a compression bomb is a
// few kilobytes on disk and gigabytes decompressed, so a single upload could
// take the container to its memory limit and kill every in-flight request
// with it. Uploads are key-gated, so this is a robustness bound rather than a
// public attack surface — but an .epub is a file from the internet either way,
// and the bound costs nothing.
const maxZipEntry = 32 << 20 // 32 MiB

// readZip reads one entry, by exact name, from a zip archive.
func readZip(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			// Read one byte past the cap so hitting it exactly is
			// distinguishable from a file that merely ends there.
			b, err := io.ReadAll(io.LimitReader(rc, maxZipEntry+1))
			if err != nil {
				return nil, err
			}
			if len(b) > maxZipEntry {
				return nil, fmt.Errorf("zip entry %q exceeds %d bytes decompressed", name, maxZipEntry)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("zip entry %q not found", name)
}

// firstNonEmpty returns the first trimmed, non-empty string in xs.
func firstNonEmpty(xs []string) string {
	for _, x := range xs {
		if t := strings.TrimSpace(x); t != "" {
			return t
		}
	}
	return ""
}
