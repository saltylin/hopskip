// Command hopskip is the single-binary daemon: it serves the embedded Vue shell,
// the inventory + session REST API, and the terminal/chat WebSockets; it owns
// the tmux sessions and the SQLite store. See CLAUDE.md.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/saltylin/hopskip/internal/agent"
	"github.com/saltylin/hopskip/internal/server"
	"github.com/saltylin/hopskip/internal/session"
	"github.com/saltylin/hopskip/internal/store"
	"github.com/saltylin/hopskip/web"
)

// env reads a bootstrap-only setting. ONLY config that cannot live in the DB
// belongs here — the listen address, the SQLite path, the tmux binary, and the
// optional dev web overlay. Everything operator-tunable (the network proxy,
// step/retry budgets, provider tokens) is set in the Settings UI and stored in
// SQLite; there is no env fallback for those.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("hopskip: ")

	var (
		addr    = env("HOPSKIP_ADDR", "127.0.0.1:8765")
		dbPath  = env("HOPSKIP_DB_PATH", "hopskip.db")
		tmuxBin = env("HOPSKIP_TMUX_BIN", "tmux")
		webDir  = os.Getenv("HOPSKIP_WEB_DIR") // optional dev overlay; empty = embed-only
	)

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store %s: %v", dbPath, err)
	}
	defer st.Close()

	mgr := session.NewManager(tmuxBin, st)
	if err := mgr.Available(); err != nil {
		log.Fatalf("%v\n  install tmux (brew install tmux) or set HOPSKIP_TMUX_BIN", err)
	}
	// Reconcile DB sessions against live tmux: mark vanished panes closed.
	live := mgr.LiveTmuxNames()
	if err := st.MarkStaleSessionsClosed(func(name string) bool { return live[name] }); err != nil {
		log.Printf("reconcile sessions: %v", err)
	}

	// Provider tokens and the network proxy come from Settings (SQLite), not env.
	// The proxy is resolved per request, so changing it takes effect with no restart.
	ag := agent.New(mgr, st, web.FS())
	srv := server.New(st, mgr, ag, web.FS(), webDir)

	log.Printf("db=%s tmux=%s web_dir=%q agent_configured=%v", dbPath, tmuxBin, webDir, ag.Configured())
	log.Printf("listening on http://%s", addr)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
