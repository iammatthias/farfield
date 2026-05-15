// Package records is the typed-records service engine.
//
// It assembles store + schema + httpkit into the records HTTP API on the
// standard library's net/http. The content and feed apps are thin binaries
// over Serve — same engine, different DB, schema directory, and address.
//
// Endpoints:
//
//	GET    /                         service index
//	GET    /status                   health + cursor
//	GET    /collections              collections + display metadata
//	GET    /schemas  /schemas/{c}     published schemas
//	GET    /records/{c}              list (ETag; ?since=<seq> cursor)
//	GET    /records/{c}/{rkey}        one record (ETag = CID)
//	POST   /records/{c}              create, server-assigned rkey   (auth)
//	PUT    /records/{c}/{rkey}        create or replace              (auth)
//	DELETE /records/{c}/{rkey}        delete                         (auth)
package records

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/core"
	"github.com/iammatthias/farfield/lib/httpkit"
	"github.com/iammatthias/farfield/lib/schema"
	"github.com/iammatthias/farfield/lib/store"
)

// Config configures one records service instance.
type Config struct {
	Addr        string
	DBPath      string
	SchemaDir   string
	Tokens      []string
	ServiceName string
}

// Service is a running records engine.
type Service struct {
	store   *store.Store
	schemas *schema.Set
	tokens  []string
	name    string
}

// Serve loads the store + schemas and serves until the process is killed.
func Serve(cfg Config) error {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", cfg.DBPath, err)
	}
	sc, err := schema.Load(cfg.SchemaDir)
	if err != nil {
		return fmt.Errorf("loading schemas from %s: %w", cfg.SchemaDir, err)
	}
	svc := &Service{store: st, schemas: sc, tokens: cfg.Tokens, name: cfg.ServiceName}

	names := make([]string, 0)
	for _, c := range sc.Collections() {
		names = append(names, c.Name)
	}
	log.Printf("farfield-%s listening on http://%s — collections: %s",
		cfg.ServiceName, cfg.Addr, strings.Join(names, ", "))
	return http.ListenAndServe(cfg.Addr, svc.Handler())
}

// Handler builds the service's HTTP handler.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.root)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /collections", s.collections)
	mux.HandleFunc("GET /schemas", s.schemasAll)
	mux.HandleFunc("GET /schemas/{collection}", s.schemaOne)
	mux.HandleFunc("GET /records/{collection}", s.list)
	mux.HandleFunc("POST /records/{collection}", s.create)
	mux.HandleFunc("GET /records/{collection}/{rkey}", s.getOne)
	mux.HandleFunc("PUT /records/{collection}/{rkey}", s.putOne)
	mux.HandleFunc("DELETE /records/{collection}/{rkey}", s.deleteOne)
	return mux
}

// ---------- read handlers --------------------------------------------------

func (s *Service) root(w http.ResponseWriter, _ *http.Request) {
	names := make([]string, 0)
	for _, c := range s.schemas.Collections() {
		names = append(names, c.Name)
	}
	httpkit.WriteJSON(w, 200, map[string]any{
		"service": "farfield-" + s.name,
		"ok":      true,
		"endpoints": []string{
			"GET    /status",
			"GET    /collections",
			"GET    /schemas",
			"GET    /records/{collection}",
			"GET    /records/{collection}?since={seq}",
			"GET    /records/{collection}/{rkey}",
			"POST   /records/{collection}            (auth)",
			"PUT    /records/{collection}/{rkey}     (auth)",
			"DELETE /records/{collection}/{rkey}     (auth)",
		},
		"collections": names,
	})
}

func (s *Service) status(w http.ResponseWriter, _ *http.Request) {
	cursor, _ := s.store.CurrentSeq()
	names := make([]string, 0)
	for _, c := range s.schemas.Collections() {
		names = append(names, c.Name)
	}
	httpkit.WriteJSON(w, 200, map[string]any{
		"service": s.name, "ok": true, "cursor": cursor, "collections": names,
	})
}

func (s *Service) collections(w http.ResponseWriter, _ *http.Request) {
	httpkit.WriteJSON(w, 200, map[string]any{"collections": s.schemas.Collections()})
}

func (s *Service) schemasAll(w http.ResponseWriter, _ *http.Request) {
	var all []schema.Schema
	for _, c := range s.schemas.Collections() {
		if sc, ok := s.schemas.SchemaFor(c.Name); ok {
			all = append(all, sc)
		}
	}
	httpkit.WriteJSON(w, 200, map[string]any{"schemas": all})
}

func (s *Service) schemaOne(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.schemas.SchemaFor(r.PathValue("collection"))
	if !ok {
		httpkit.WriteError(w, httpkit.NotFound("unknown collection `"+r.PathValue("collection")+"`"))
		return
	}
	httpkit.WriteJSON(w, 200, sc)
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	collection := r.PathValue("collection")
	if !s.known(collection) {
		httpkit.WriteError(w, httpkit.NotFound("unknown collection `"+collection+"`"))
		return
	}
	if since := r.URL.Query().Get("since"); since != "" {
		n, err := strconv.ParseInt(since, 10, 64)
		if err != nil {
			httpkit.WriteError(w, httpkit.BadRequest("invalid_cursor", "?since must be an integer seq"))
			return
		}
		recs, tombs, err := s.store.ChangedSince(collection, n)
		if err != nil {
			httpkit.WriteError(w, httpkit.Internal(err.Error()))
			return
		}
		cursor, _ := s.store.CurrentSeq()
		httpkit.WriteJSON(w, 200, map[string]any{
			"records": recs, "deletions": tombs, "cursor": cursor,
		})
		return
	}
	etag, err := s.store.ListETag(collection)
	if err != nil {
		httpkit.WriteError(w, httpkit.Internal(err.Error()))
		return
	}
	if etag != "" && ifNoneMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	recs, err := s.store.List(collection)
	if err != nil {
		httpkit.WriteError(w, httpkit.Internal(err.Error()))
		return
	}
	cursor, _ := s.store.CurrentSeq()
	if etag != "" {
		w.Header().Set("ETag", `"`+etag+`"`)
	}
	httpkit.WriteJSON(w, 200, map[string]any{"records": recs, "cursor": cursor})
}

func (s *Service) getOne(w http.ResponseWriter, r *http.Request) {
	collection, rkey := r.PathValue("collection"), r.PathValue("rkey")
	rec, err := s.store.Get(collection, rkey)
	if err != nil {
		httpkit.WriteError(w, httpkit.Internal(err.Error()))
		return
	}
	if rec == nil {
		httpkit.WriteError(w, httpkit.NotFound(collection+"/"+rkey))
		return
	}
	if ifNoneMatch(r, rec.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", `"`+rec.CID+`"`)
	httpkit.WriteJSON(w, 200, rec)
}

// ---------- write handlers -------------------------------------------------

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	if e := httpkit.VerifyBearer(r, s.tokens); e != nil {
		httpkit.WriteError(w, e)
		return
	}
	rkey := strconv.FormatInt(time.Now().UnixMilli(), 10)
	s.writeRecord(w, r, r.PathValue("collection"), rkey)
}

func (s *Service) putOne(w http.ResponseWriter, r *http.Request) {
	if e := httpkit.VerifyBearer(r, s.tokens); e != nil {
		httpkit.WriteError(w, e)
		return
	}
	s.writeRecord(w, r, r.PathValue("collection"), r.PathValue("rkey"))
}

func (s *Service) deleteOne(w http.ResponseWriter, r *http.Request) {
	if e := httpkit.VerifyBearer(r, s.tokens); e != nil {
		httpkit.WriteError(w, e)
		return
	}
	collection, rkey := r.PathValue("collection"), r.PathValue("rkey")
	if !s.known(collection) {
		httpkit.WriteError(w, httpkit.NotFound("unknown collection `"+collection+"`"))
		return
	}
	removed, err := s.store.Delete(collection, rkey)
	if err != nil {
		httpkit.WriteError(w, httpkit.Internal(err.Error()))
		return
	}
	if !removed {
		httpkit.WriteError(w, httpkit.NotFound(collection+"/"+rkey))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) writeRecord(w http.ResponseWriter, r *http.Request, collection, rkey string) {
	if !s.known(collection) {
		httpkit.WriteError(w, &httpkit.APIError{
			Status: http.StatusNotFound, Code: "unknown_collection",
			Message: "unknown collection `" + collection + "`",
		})
		return
	}
	if !validRkey(rkey) {
		httpkit.WriteError(w, httpkit.BadRequest("invalid_rkey",
			"rkey `"+rkey+"` must match [a-z0-9-]{1,128}"))
		return
	}
	var value map[string]any
	if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
		httpkit.WriteError(w, httpkit.BadRequest("invalid_json", "request body is not valid JSON"))
		return
	}
	if err := s.schemas.Validate(collection, value); err != nil {
		httpkit.WriteError(w, httpkit.BadRequest("invalid_record", err.Error()))
		return
	}
	res, err := s.store.Put(collection, rkey, core.Record(value))
	if err != nil {
		httpkit.WriteError(w, httpkit.Internal(err.Error()))
		return
	}
	status := http.StatusOK
	if res.Status == "created" {
		status = http.StatusCreated
	}
	var seq any
	if res.Status != "unchanged" {
		seq = res.Seq
	}
	httpkit.WriteJSON(w, status, map[string]any{
		"collection": collection, "rkey": rkey, "cid": res.CID, "seq": seq,
	})
}

// ---------- helpers --------------------------------------------------------

func (s *Service) known(collection string) bool {
	_, ok := s.schemas.Collection(collection)
	return ok
}

func ifNoneMatch(r *http.Request, etag string) bool {
	v := strings.TrimSpace(r.Header.Get("If-None-Match"))
	return v == "*" || strings.Trim(v, `"`) == etag
}

// validRkey enforces the rkey grammar: [a-z0-9-]{1,128}.
func validRkey(rkey string) bool {
	if len(rkey) < 1 || len(rkey) > 128 {
		return false
	}
	for _, c := range rkey {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// TokensFromEnv reads FARFIELD_TOKEN (+ FARFIELD_TOKEN_PREVIOUS) from the
// environment, falling back to a dev token with a warning.
func TokensFromEnv() []string {
	var tokens []string
	for _, v := range []string{"FARFIELD_TOKEN", "FARFIELD_TOKEN_PREVIOUS"} {
		if t := os.Getenv(v); t != "" {
			tokens = append(tokens, t)
		}
	}
	if len(tokens) == 0 {
		log.Println("warning: FARFIELD_TOKEN unset — using 'dev-token' (local dev only)")
		tokens = []string{"dev-token"}
	}
	return tokens
}
