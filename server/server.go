package server

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kunde21/paclan/config"
	"golang.org/x/sync/errgroup"
)

const (
	ARCH_HEADER        = `X-Arch-Req`
	PACMAN_CONFIG_FILE = `/etc/pacman.conf`
)

type server struct {
	*http.Server
	// *peers.DNS
	PeerLister
	arch  string
	cache string
	sync  string
}

type PeerLister interface {
	GetPeerList() []string
}

func New(conf *config.Paclan, peers PeerLister) (server, error) {
	srv := server{
		PeerLister: peers,
		arch:       conf.Arch,
		cache:      conf.CacheDir,
		sync:       conf.SyncDir,
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(srv.handle))
	srv.Server = &http.Server{
		Addr:              net.JoinHostPort("", conf.HTTPPort),
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return srv, nil
}

func (srv server) handle(w http.ResponseWriter, r *http.Request) {
	addr, err := net.ResolveTCPAddr("tcp", r.RemoteAddr)
	if err != nil {
		log.Printf("Error serving %s: %s\n", r.RemoteAddr, err)
		return
	}
	if addr.IP.IsLoopback() {
		srv.handleLocal(w, r)
	} else {
		srv.handleRemote(w, r)
	}
}

func (srv server) handleLocal(w http.ResponseWriter, r *http.Request) {
	log.Println("local request:", r.URL.Path)
	// sync indicates a database master server, do not fetch from peers
	if srv.sync != "" && strings.HasSuffix(filepath.Base(r.URL.Path), ".db") {
		log.Println("database master", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	found := make(chan string, 1)
	defer close(found)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	redir := srv.Search(ctx, r.URL)
	if redir == "" {
		log.Println("not found", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	log.Println("found", r.URL, redir)
	switch r.Method {
	case http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		http.Redirect(w, r, redir, http.StatusFound)
		return
	}
}

func (srv server) handleRemote(w http.ResponseWriter, r *http.Request) {
	log.Println("remote request:", path.Base(r.URL.Path))
	file := path.Base(r.URL.Path)
	if arch := r.Header.Get(ARCH_HEADER); arch != "" && srv.arch != arch {
		log.Println("pkg search:", arch, file, "arch mismatch")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	fpath := srv.findFile(file)
	if fpath == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	log.Println("pkg found:", file)
	switch r.Method {
	case http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		http.ServeFile(w, r, fpath)
	}
}

// findFile checks for cached package or database (if enabled)
// absolute path returned if found.
func (srv server) findFile(file string) string {
	fpath := filepath.Join(srv.cache, file)
	_, err := os.Stat(fpath)
	if err == nil {
		return fpath
	}
	if srv.sync == "" {
		log.Println("pkg search:", file, err)
		return ""
	}
	fpath = filepath.Join(srv.sync, file)
	_, err = os.Stat(fpath)
	if err == nil {
		return fpath
	}
	log.Println("pkg search:", file, err)
	return ""
}

// Search for a package in the peer network.
func (srv server) Search(ctx context.Context, r *url.URL) (host string) {
	newUrl := *r
	newUrl.Scheme = "http"
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	eg, ctx := errgroup.WithContext(ctx)
	found := make(chan string, 1)
	peers := srv.GetPeerList()
	log.Printf("requesting pkg %q from %d peers", path.Base(r.Path), len(peers))
	for _, peer := range peers {
		newUrl.Host = peer
		urlNext := newUrl.String()
		eg.Go(func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, urlNext, nil)
			if err != nil {
				return nil
			}
			req.Header.Add(ARCH_HEADER, srv.arch)
			resp, err := http.DefaultClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				select {
				case found <- urlNext:
					cancel() // use first found instance
				default:
				}
			}
			return nil
		})
	}
	go func() { // peform cleanup in the background
		eg.Wait()
		close(found)
	}()
	return <-found
}
