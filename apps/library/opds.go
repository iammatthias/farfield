package main

import (
	"encoding/xml"
	"fmt"
)

// OPDS 1.2 namespaces, link relations, and media types. dc:* uses the Dublin
// Core elements namespace, the one e-readers expect for language/identifier.
const (
	atomNS = "http://www.w3.org/2005/Atom"
	dcNS   = "http://purl.org/dc/elements/1.1/"
	opdsNS = "http://opds-spec.org/2010/catalog"

	relSelf        = "self"
	relStart       = "start"
	relSubsection  = "subsection"
	relAcquisition = "http://opds-spec.org/acquisition"
	relImage       = "http://opds-spec.org/image"
	relThumbnail   = "http://opds-spec.org/image/thumbnail"

	// feedType is an OPDS acquisition feed (a list of books); navFeedType is a
	// navigation feed (a list of subsections — folders linking to other feeds).
	feedType    = "application/atom+xml;profile=opds-catalog;kind=acquisition"
	navFeedType = "application/atom+xml;profile=opds-catalog;kind=navigation"
	epubMime    = "application/epub+zip"
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

// opdsContent is an Atom <content type="text"> element — used on navigation
// entries to note a folder's book count.
type opdsContent struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

// opdsEntry is one acquisition entry — a downloadable book. The dc:* fields
// carry their prefix in the local name so encoding/xml emits them verbatim
// under the xmlns:dc declared on the feed.
type opdsEntry struct {
	Title      string       `xml:"title"`
	ID         string       `xml:"id"`
	Updated    string       `xml:"updated"`
	Author     *opdsAuthor  `xml:"author,omitempty"`
	Language   string       `xml:"dc:language,omitempty"`
	Identifier string       `xml:"dc:identifier,omitempty"`
	Summary    string       `xml:"summary,omitempty"`
	Content    *opdsContent `xml:"content,omitempty"`
	Links      []opdsLink   `xml:"link"`
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

// catalogXML renders an OPDS acquisition feed for books. title is the feed's
// title; selfHref is its own path; updated is the RFC3339 <updated> stamp. The
// start link points back at the navigation root.
func catalogXML(books []Book, title, selfHref, updated string) ([]byte, error) {
	feed := opdsFeed{
		Xmlns:     atomNS,
		XmlnsDC:   dcNS,
		XmlnsOPDS: opdsNS,
		ID:        "urn:farfield:library:" + selfHref,
		Title:     title,
		Updated:   updated,
		Links: []opdsLink{
			{Rel: relSelf, Href: selfHref, Type: feedType},
			{Rel: relStart, Href: "/opds", Type: navFeedType},
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

// NavItem is one entry in the navigation feed — a folder linking to its own
// acquisition feed of books.
type NavItem struct {
	Title string
	Href  string
	Count int
}

// navFeedXML renders an OPDS navigation feed: each item is a subsection link to
// an acquisition feed, with the folder's book count as its content.
func navFeedXML(items []NavItem, selfHref, updated string) ([]byte, error) {
	feed := opdsFeed{
		Xmlns:     atomNS,
		XmlnsDC:   dcNS,
		XmlnsOPDS: opdsNS,
		ID:        "urn:farfield:library:nav",
		Title:     "farfield · library",
		Updated:   updated,
		Links: []opdsLink{
			{Rel: relSelf, Href: selfHref, Type: navFeedType},
			{Rel: relStart, Href: "/opds", Type: navFeedType},
		},
	}
	for _, it := range items {
		e := opdsEntry{
			Title:   it.Title,
			ID:      "urn:farfield:library:nav:" + it.Href,
			Updated: updated,
			Links:   []opdsLink{{Rel: relSubsection, Href: it.Href, Type: feedType}},
		}
		unit := "books"
		if it.Count == 1 {
			unit = "book"
		}
		e.Content = &opdsContent{Type: "text", Text: fmt.Sprintf("%d %s", it.Count, unit)}
		feed.Entries = append(feed.Entries, e)
	}
	body, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}
