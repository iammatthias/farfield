package main

import (
	"errors"
	"net/http"
	"strings"
)

// Collections as the author manages them — create, rename, delete. A
// collection is just a namespace for entries; deleting one is refused while it
// still holds any.

func (s *Server) handleNewCollection(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "collection_form.html", map[string]any{
		"IsNew": true, "Action": "/collections", "Collection": Collection{},
	})
}

func (s *Server) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	c := &Collection{
		Name:        strings.TrimSpace(r.FormValue("name")),
		Slug:        firstNonEmpty(slugify(r.FormValue("slug")), slugify(r.FormValue("name"))),
		Description: strings.TrimSpace(r.FormValue("description")),
	}
	if c.Name == "" || c.Slug == "" {
		s.renderCollectionForm(w, c, true, "/collections", "Name is required.")
		return
	}
	if err := insertCollection(s.db, c); err != nil {
		if errors.Is(err, errSlugTaken) {
			s.renderCollectionForm(w, c, true, "/collections", err.Error())
			return
		}
		s.fail(w, "create collection", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleEditCollection(w http.ResponseWriter, r *http.Request) {
	c, err := getCollection(s.db, r.PathValue("slug"))
	if err != nil {
		s.fail(w, "get collection", err)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	s.renderCollectionForm(w, c, false, "/collections/"+c.Slug, "")
}

func (s *Server) handleUpdateCollection(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	desc := strings.TrimSpace(r.FormValue("description"))
	if name == "" {
		c := &Collection{Slug: slug, Name: name, Description: desc}
		s.renderCollectionForm(w, c, false, "/collections/"+slug, "Name is required.")
		return
	}
	ok, err := updateCollection(s.db, slug, name, desc)
	if err != nil {
		s.fail(w, "update collection", err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteCollection(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "delete collection", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
