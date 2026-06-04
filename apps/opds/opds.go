package main

import "encoding/xml"

// OPDS 1.2 namespaces, link relations, and media types. dc:* uses the Dublin
// Core elements namespace, the one e-readers expect for language/identifier.
const (
	atomNS = "http://www.w3.org/2005/Atom"
	dcNS   = "http://purl.org/dc/elements/1.1/"
	opdsNS = "http://opds-spec.org/2010/catalog"

	relSelf        = "self"
	relStart       = "start"
	relAcquisition = "http://opds-spec.org/acquisition"
	relImage       = "http://opds-spec.org/image"
	relThumbnail   = "http://opds-spec.org/image/thumbnail"

	// feedType is the content type of an OPDS acquisition feed.
	feedType = "application/atom+xml;profile=opds-catalog;kind=acquisition"
	epubMime = "application/epub+zip"
)

// opdsLink is an Atom <link> with the rel/href/type an OPDS reader keys on.
type opdsLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
	Type string `xml:"type,attr"`
}

// opdsAuthor is an Atom <author> element.
type opdsAuthor struct {
	Name string `xml:"name"`
}

// opdsEntry is one acquisition entry — a downloadable book. The dc:* fields
// carry their prefix in the local name so encoding/xml emits them verbatim
// under the xmlns:dc declared on the feed.
type opdsEntry struct {
	Title      string      `xml:"title"`
	ID         string      `xml:"id"`
	Updated    string      `xml:"updated"`
	Author     *opdsAuthor `xml:"author,omitempty"`
	Language   string      `xml:"dc:language,omitempty"`
	Identifier string      `xml:"dc:identifier,omitempty"`
	Summary    string      `xml:"summary,omitempty"`
	Links      []opdsLink  `xml:"link"`
}

// opdsFeed is the root acquisition feed. The xmlns:* attributes are emitted
// literally; XMLName carries no namespace so encoding/xml does not inject its
// own, letting the dc:/opds: prefixes resolve as written.
type opdsFeed struct {
	XMLName   xml.Name    `xml:"feed"`
	Xmlns     string      `xml:"xmlns,attr"`
	XmlnsDC   string      `xml:"xmlns:dc,attr"`
	XmlnsOPDS string      `xml:"xmlns:opds,attr"`
	ID        string      `xml:"id"`
	Title     string      `xml:"title"`
	Updated   string      `xml:"updated"`
	Links     []opdsLink  `xml:"link"`
	Entries   []opdsEntry `xml:"entry"`
}

// catalogXML renders an OPDS acquisition feed for books. selfHref is the
// catalog's own path (used for the self/start links); updated is the feed's
// RFC3339 <updated> timestamp.
func catalogXML(books []Book, selfHref, updated string) ([]byte, error) {
	feed := opdsFeed{
		Xmlns:     atomNS,
		XmlnsDC:   dcNS,
		XmlnsOPDS: opdsNS,
		ID:        "urn:farfield:opds",
		Title:     "farfield · opds",
		Updated:   updated,
		Links: []opdsLink{
			{Rel: relSelf, Href: selfHref, Type: feedType},
			{Rel: relStart, Href: selfHref, Type: feedType},
		},
	}

	for _, b := range books {
		e := opdsEntry{
			Title:      b.Title,
			ID:         "urn:cid:" + b.CID,
			Updated:    b.CreatedAt,
			Language:   b.Language,
			Identifier: b.Identifier,
			Summary:    b.Description,
			Links: []opdsLink{
				{Rel: relAcquisition, Href: "/opds/download/" + b.CID, Type: epubMime},
			},
		}
		if b.Author != "" {
			e.Author = &opdsAuthor{Name: b.Author}
		}
		if b.CoverCID != "" {
			ct := b.CoverMime
			if ct == "" {
				ct = "image/jpeg"
			}
			e.Links = append(e.Links,
				opdsLink{Rel: relImage, Href: "/opds/cover/" + b.CoverCID, Type: ct},
				opdsLink{Rel: relThumbnail, Href: "/opds/cover/" + b.CoverCID, Type: ct},
			)
		}
		feed.Entries = append(feed.Entries, e)
	}

	body, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}
