// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package frontend

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v7"
	"golang.org/x/discovery/internal"
	"golang.org/x/discovery/internal/config"
	"golang.org/x/discovery/internal/derrors"
	"golang.org/x/discovery/internal/license"
	"golang.org/x/discovery/internal/log"
	"golang.org/x/discovery/internal/middleware"
)

// Server can be installed to serve the go discovery frontend.
type Server struct {
	ds internal.DataSource
	// cmplClient is a redis client that has access to the "completions" sorted
	// set.
	cmplClient      *redis.Client
	staticPath      string
	templateDir     string
	reloadTemplates bool
	errorPage       []byte

	mu        sync.Mutex // Protects all fields below
	templates map[string]*template.Template
}

// NewServer creates a new Server for the given database and template directory.
// reloadTemplates should be used during development when it can be helpful to
// reload templates from disk each time a page is loaded.
func NewServer(ds internal.DataSource, cmplClient *redis.Client, staticPath string, reloadTemplates bool) (*Server, error) {
	templateDir := filepath.Join(staticPath, "html")
	ts, err := parsePageTemplates(templateDir)
	if err != nil {
		return nil, fmt.Errorf("error parsing templates: %v", err)
	}
	s := &Server{
		ds:              ds,
		cmplClient:      cmplClient,
		staticPath:      staticPath,
		templateDir:     templateDir,
		reloadTemplates: reloadTemplates,
		templates:       ts,
	}
	errorPageBytes, err := s.renderErrorPage(http.StatusInternalServerError, nil)
	if err != nil {
		return nil, fmt.Errorf("s.renderErrorPage(http.StatusInternalServerError, nil): %v", err)
	}
	s.errorPage = errorPageBytes
	return s, nil
}

// Install registers server routes using the given handler registration func.
func (s *Server) Install(handle func(string, http.Handler), redisClient *redis.Client) {
	var (
		modHandler    http.Handler = http.HandlerFunc(s.handleModuleDetails)
		detailHandler http.Handler = http.HandlerFunc(s.handleDetails)
		searchHandler http.Handler = http.HandlerFunc(s.handleSearch)
	)
	if redisClient != nil {
		modHandler = middleware.Cache("module-details", redisClient, moduleTTL)(modHandler)
		detailHandler = middleware.Cache("package-details", redisClient, packageTTL)(detailHandler)
		searchHandler = middleware.Cache("search", redisClient, middleware.TTL(defaultTTL))(searchHandler)
	}
	handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(s.staticPath))))
	handle("/favicon.ico", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, fmt.Sprintf("%s/img/favicon.ico", http.Dir(s.staticPath)))
	}))
	handle("/mod/", modHandler)
	handle("/pkg/", http.HandlerFunc(s.handlePackageDetailsRedirect))
	handle("/search", searchHandler)
	handle("/search-help", s.staticPageHandler("search_help.tmpl", "Search Help - go.dev"))
	handle("/license-policy", s.licensePolicyHandler())
	handle("/", detailHandler)
	handle("/autocomplete", http.HandlerFunc(s.handleAutoCompletion))
	handle("/robots.txt", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(`User-agent: *
Disallow: /*?tab=*
Disallow: /search?*
Disallow: /mod/
Disallow: /pkg/
`))
	}))
}

const (
	// defaultTTL is used when details tab contents are subject to change, or when
	// there is a problem confirming that the details can be permanently cached.
	defaultTTL = 1 * time.Hour
	// shortTTL is used for volatile content, such as the latest version of a
	// package or module.
	shortTTL = 10 * time.Minute
	// longTTL is used when details content is essentially static.
	longTTL = 24 * time.Hour
)

// packageTTL assigns the cache TTL for package detail requests.
func packageTTL(r *http.Request) time.Duration {
	return detailsTTL(r.URL.Path, r.FormValue("tab"))
}

// moduleTTL assigns the cache TTL for /mod/ requests.
func moduleTTL(r *http.Request) time.Duration {
	urlPath := strings.TrimPrefix(r.URL.Path, "/mod")
	return detailsTTL(urlPath, r.FormValue("tab"))
}

func detailsTTL(urlPath, tab string) time.Duration {
	if urlPath == "/" {
		return defaultTTL
	}
	_, _, version, err := parseDetailsURLPath(urlPath)
	if err != nil {
		log.Errorf("falling back to default module TTL: %v", err)
		return defaultTTL
	}
	if version == internal.LatestVersion {
		return shortTTL
	}
	if tab == "importedby" || tab == "versions" {
		return defaultTTL
	}
	return longTTL
}

// TagRoute categorizes incoming requests to the frontend for use in
// monitoring.
func TagRoute(route string, r *http.Request) string {
	tag := strings.Trim(route, "/")
	if tab := r.FormValue("tab"); tab != "" {
		// Verify that the tab value actually exists, otherwise this is unsanitized
		// input and could result in unbounded cardinality in our metrics.
		_, pkgOK := packageTabLookup[tab]
		_, modOK := moduleTabLookup[tab]
		if pkgOK || modOK {
			if tag != "" {
				tag += "-"
			}
			tag += tab
		}
	}
	return tag
}

func suggestedSearch(userInput string) template.HTML {
	safe := template.HTMLEscapeString(userInput)
	return template.HTML(fmt.Sprintf(`To search for packages like %q, <a href="/search?q=%s">click here</a>.</p>`, safe, safe))
}

// staticPageHandler handles requests to a template that contains no dynamic
// content.
func (s *Server) staticPageHandler(templateName, title string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.servePage(w, templateName, newBasePage(r, title))
	}
}

// basePage contains fields shared by all pages when rendering templates.
type basePage struct {
	HTMLTitle string
	Query     string
	Nonce     string
	Experiments *Experiments
}

// Experiments is a placeholder for a handle that can be used to interrogate
// experiments. The actual experiment functionality is being implemented in
// b/146052411.
// TODO(b/146052411): make this real
type Experiments struct {}

// Active reports whether the experiment id is active.
func (e *Experiments) Active(id string) bool {
	// Return false so that all experiments are disabled. This is just a
	// placeholder so that we can merge experiment-gated features while the
	// experiments middleware is being implemented.
	return false
}

// licensePolicyPage is used to generate the static license policy page.
type licensePolicyPage struct {
	basePage
	LicenseFileNames, LicenseTypes []string
}

func (s *Server) licensePolicyHandler() http.HandlerFunc {
	fileNames := license.FileNames()
	licenses := license.AcceptedOSILicenses()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := licensePolicyPage{
			basePage:         newBasePage(r, "Licenses - go.dev"),
			LicenseFileNames: fileNames,
			LicenseTypes:     licenses,
		}
		s.servePage(w, "license_policy.tmpl", page)
	})
}

// newBasePage returns a base page for the given request and title.
func newBasePage(r *http.Request, title string) basePage {
	return basePage{
		HTMLTitle: title,
		Query:     searchQuery(r),
		Nonce:     middleware.NoncePlaceholder,
	}
}

// GoogleAnalyticsTrackingID returns the tracking ID from
// func (b basePage) GoogleAnalyticsTrackingID() string {
	return "UA-141356704-1"
}

// AppVersionLabel uniquely identifies the currently running binary. It can be
// used for cache-busting query parameters.
func (b basePage) AppVersionLabel() string {
	return config.AppVersionLabel()
}

// errorPage contains fields for rendering a HTTP error page.
type errorPage struct {
	basePage
	Message          string
	SecondaryMessage template.HTML
}

// PanicHandler returns an http.HandlerFunc that can be used in HTTP
// middleware. It returns an error if something goes wrong pre-rendering the
// error template.
func (s *Server) PanicHandler() (_ http.HandlerFunc, err error) {
	defer derrors.Wrap(&err, "PanicHandler")
	status := http.StatusInternalServerError
	buf, err := s.renderErrorPage(status, nil)
	if err != nil {
		return nil, err
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if _, err := io.Copy(w, bytes.NewReader(buf)); err != nil {
			log.Errorf("Error copying panic template to ResponseWriter: %v", err)
		}
	}, nil
}

func (s *Server) serveErrorPage(w http.ResponseWriter, r *http.Request, status int, page *errorPage) {
	if page == nil {
		page = &errorPage{
			basePage: newBasePage(r, ""),
		}
	}
	buf, err := s.renderErrorPage(status, page)
	if err != nil {
		log.Errorf("s.renderErrorPage(w, %d, %v): %v", status, page, err)
		buf = s.errorPage
		status = http.StatusInternalServerError
	}

	w.WriteHeader(status)
	if _, err := io.Copy(w, bytes.NewReader(buf)); err != nil {
		log.Errorf("Error copying template %q buffer to ResponseWriter: %v", "error.tmpl", err)
	}
}

// renderErrorPage executes error.tmpl with the given errorPage
func (s *Server) renderErrorPage(status int, page *errorPage) ([]byte, error) {
	statusInfo := fmt.Sprintf("%d %s", status, http.StatusText(status))
	if page == nil {
		page = &errorPage{
			Message: statusInfo,
			basePage: basePage{
				HTMLTitle: statusInfo,
				Nonce:     middleware.NoncePlaceholder,
			},
		}
	}
	if page.Message == "" {
		page.Message = statusInfo
	}
	if page.HTMLTitle == "" {
		page.HTMLTitle = statusInfo
	}
	return s.renderPage("error.tmpl", page)
}

// servePage is used to execute all templates for a *Server.
func (s *Server) servePage(w http.ResponseWriter, templateName string, page interface{}) {
	buf, err := s.renderPage(templateName, page)
	if err != nil {
		log.Errorf("s.renderPage(%q, %+v): %v", templateName, page, err)
		w.WriteHeader(http.StatusInternalServerError)
		buf = s.errorPage
	}
	if _, err := io.Copy(w, bytes.NewReader(buf)); err != nil {
		log.Errorf("Error copying template %q buffer to ResponseWriter: %v", templateName, err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// renderPage executes the given templateName with page.
func (s *Server) renderPage(templateName string, page interface{}) ([]byte, error) {
	if s.reloadTemplates {
		s.mu.Lock()
		defer s.mu.Unlock()
		var err error
		s.templates, err = parsePageTemplates(s.templateDir)
		if err != nil {
			return nil, fmt.Errorf("error parsing templates: %v", err)
		}
	}

	var buf bytes.Buffer
	tmpl := s.templates[templateName]
	if tmpl == nil {
		return nil, fmt.Errorf("BUG: s.templates[%q] not found", templateName)
	}
	if err := tmpl.Execute(&buf, page); err != nil {
		log.Errorf("Error executing page template %q: %v", templateName, err)
		return nil, err

	}
	return buf.Bytes(), nil
}

// parsePageTemplates parses html templates contained in the given base
// directory in order to generate a map of Name->*template.Template.
//
// Separate templates are used so that certain contextual functions (e.g.
// templateName) can be bound independently for each page.
func parsePageTemplates(base string) (map[string]*template.Template, error) {
	htmlSets := [][]string{
		{"index.tmpl"},
		{"error.tmpl"},
		{"search.tmpl"},
		{"search_help.tmpl"},
		{"license_policy.tmpl"},
		{"overview.tmpl", "details.tmpl"},
		{"subdirectories.tmpl", "details.tmpl"},
		{"pkg_doc.tmpl", "details.tmpl"},
		{"pkg_importedby.tmpl", "details.tmpl"},
		{"pkg_imports.tmpl", "details.tmpl"},
		{"licenses.tmpl", "details.tmpl"},
		{"versions.tmpl", "details.tmpl"},
		{"not_implemented.tmpl", "details.tmpl"},
	}

	templates := make(map[string]*template.Template)
	for _, set := range htmlSets {
		t, err := template.New("base.tmpl").Funcs(template.FuncMap{
			"add": func(i, j int) int { return i + j },
			"pluralize": func(i int, s string) string {
				if i == 1 {
					return s
				}
				return s + "s"
			},
			"commaseparate": func(s []string) string {
				return strings.Join(s, ", ")
			},
		}).ParseFiles(filepath.Join(base, "base.tmpl"))
		if err != nil {
			return nil, fmt.Errorf("ParseFiles: %v", err)
		}
		helperGlob := filepath.Join(base, "helpers", "*.tmpl")
		if _, err := t.ParseGlob(helperGlob); err != nil {
			return nil, fmt.Errorf("ParseGlob(%q): %v", helperGlob, err)
		}

		var files []string
		for _, f := range set {
			files = append(files, filepath.Join(base, "pages", f))
		}
		if _, err := t.ParseFiles(files...); err != nil {
			return nil, fmt.Errorf("ParseFiles(%v): %v", files, err)
		}
		templates[set[0]] = t
	}
	return templates, nil
}
