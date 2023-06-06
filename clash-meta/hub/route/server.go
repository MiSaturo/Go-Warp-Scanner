package route

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Dreamacro/clash/adapter/inbound"
	CN "github.com/Dreamacro/clash/common/net"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/log"
	"github.com/Dreamacro/clash/tunnel/statistic"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/go-chi/render"
	"github.com/gorilla/websocket"
)

var (
	serverSecret = ""
	serverAddr   = ""

	uiPath = ""

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
)

type Traffic struct {
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

type Memory struct {
	Inuse   uint64 `json:"inuse"`
	OSLimit uint64 `json:"oslimit"` // maybe we need it in the future
}

func SetUIPath(path string) {
	uiPath = C.Path.Resolve(path)
}

func Start(addr string, tlsAddr string, secret string,
	certificat, privateKey string, isDebug bool) {
	if serverAddr != "" {
		return
	}

	serverAddr = addr
	serverSecret = secret

	r := chi.NewRouter()
	corsM := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE"},
		AllowedHeaders: []string{"Content-Type", "Authorization"},
		MaxAge:         300,
	})
	r.Use(corsM.Handler)
	if isDebug {
		r.Mount("/debug", func() http.Handler {
			r := chi.NewRouter()
			r.Put("/gc", func(w http.ResponseWriter, r *http.Request) {
				debug.FreeOSMemory()
			})
			handler := middleware.Profiler
			r.Mount("/", handler())
			return r
		}())
	}
	r.Group(func(r chi.Router) {
		r.Use(authentication)
		r.Get("/", hello)
		r.Get("/logs", getLogs)
		r.Get("/traffic", traffic)
		r.Get("/memory", memory)
		r.Get("/version", version)
		r.Mount("/configs", configRouter())
		r.Mount("/proxies", proxyRouter())
		r.Mount("/group", GroupRouter())
		r.Mount("/rules", ruleRouter())
		r.Mount("/connections", connectionRouter())
		r.Mount("/providers/proxies", proxyProviderRouter())
		r.Mount("/providers/rules", ruleProviderRouter())
		r.Mount("/cache", cacheRouter())
		r.Mount("/dns", dnsRouter())
		r.Mount("/restart", restartRouter())
		r.Mount("/upgrade", upgradeRouter())

	})

	if uiPath != "" {
		r.Group(func(r chi.Router) {
			fs := http.StripPrefix("/ui", http.FileServer(http.Dir(uiPath)))
			r.Get("/ui", http.RedirectHandler("/ui/", http.StatusTemporaryRedirect).ServeHTTP)
			r.Get("/ui/*", func(w http.ResponseWriter, r *http.Request) {
				fs.ServeHTTP(w, r)
			})
		})
	}

	if len(tlsAddr) > 0 {
		go func() {
			c, err := CN.ParseCert(certificat, privateKey)
			if err != nil {
				log.Errorln("External controller tls listen error: %s", err)
				return
			}

			l, err := inbound.Listen("tcp", tlsAddr)
			if err != nil {
				log.Errorln("External controller tls listen error: %s", err)
				return
			}

			serverAddr = l.Addr().String()
			log.Infoln("RESTful API tls listening at: %s", serverAddr)
			tlsServe := &http.Server{
				Handler: r,
				TLSConfig: &tls.Config{
					Certificates: []tls.Certificate{c},
				},
			}
			if err = tlsServe.ServeTLS(l, "", ""); err != nil {
				log.Errorln("External controller tls serve error: %s", err)
			}
		}()
	}

	l, err := inbound.Listen("tcp", addr)
	if err != nil {
		log.Errorln("External controller listen error: %s", err)
		return
	}
	serverAddr = l.Addr().String()
	log.Infoln("RESTful API listening at: %s", serverAddr)

	if err = http.Serve(l, r); err != nil {
		log.Errorln("External controller serve error: %s", err)
	}

}

func authentication(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if serverSecret == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Browser websocket not support custom header
		if websocket.IsWebSocketUpgrade(r) && r.URL.Query().Get("token") != "" {
			token := r.URL.Query().Get("token")
			if token != serverSecret {
				render.Status(r, http.StatusUnauthorized)
				render.JSON(w, r, ErrUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		header := r.Header.Get("Authorization")
		bearer, token, found := strings.Cut(header, " ")

		hasInvalidHeader := bearer != "Bearer"
		hasInvalidSecret := !found || token != serverSecret
		if hasInvalidHeader || hasInvalidSecret {
			render.Status(r, http.StatusUnauthorized)
			render.JSON(w, r, ErrUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func hello(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, r, render.M{"hello": "clash.meta"})
}

func traffic(w http.ResponseWriter, r *http.Request) {
	var wsConn *websocket.Conn
	if websocket.IsWebSocketUpgrade(r) {
		var err error
		wsConn, err = upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
	}

	if wsConn == nil {
		w.Header().Set("Content-Type", "application/json")
		render.Status(r, http.StatusOK)
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	t := statistic.DefaultManager
	buf := &bytes.Buffer{}
	var err error
	for range tick.C {
		buf.Reset()
		up, down := t.Now()
		if err := json.NewEncoder(buf).Encode(Traffic{
			Up:   up,
			Down: down,
		}); err != nil {
			break
		}

		if wsConn == nil {
			_, err = w.Write(buf.Bytes())
			w.(http.Flusher).Flush()
		} else {
			err = wsConn.WriteMessage(websocket.TextMessage, buf.Bytes())
		}

		if err != nil {
			break
		}
	}
}

func memory(w http.ResponseWriter, r *http.Request) {
	var wsConn *websocket.Conn
	if websocket.IsWebSocketUpgrade(r) {
		var err error
		wsConn, err = upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
	}

	if wsConn == nil {
		w.Header().Set("Content-Type", "application/json")
		render.Status(r, http.StatusOK)
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	t := statistic.DefaultManager
	buf := &bytes.Buffer{}
	var err error
	first := true
	for range tick.C {
		buf.Reset()

		inuse := t.Memory()
		// make chat.js begin with zero
		// this is shit var,but we need output 0 for first time
		if first {
			inuse = 0
			first = false
		}
		if err := json.NewEncoder(buf).Encode(Memory{
			Inuse:   inuse,
			OSLimit: 0,
		}); err != nil {
			break
		}
		if wsConn == nil {
			_, err = w.Write(buf.Bytes())
			w.(http.Flusher).Flush()
		} else {
			err = wsConn.WriteMessage(websocket.TextMessage, buf.Bytes())
		}

		if err != nil {
			break
		}
	}
}

type Log struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

func getLogs(w http.ResponseWriter, r *http.Request) {
	levelText := r.URL.Query().Get("level")
	if levelText == "" {
		levelText = "info"
	}

	level, ok := log.LogLevelMapping[levelText]
	if !ok {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, ErrBadRequest)
		return
	}

	var wsConn *websocket.Conn
	if websocket.IsWebSocketUpgrade(r) {
		var err error
		wsConn, err = upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
	}

	if wsConn == nil {
		w.Header().Set("Content-Type", "application/json")
		render.Status(r, http.StatusOK)
	}

	ch := make(chan log.Event, 1024)
	sub := log.Subscribe()
	defer log.UnSubscribe(sub)
	buf := &bytes.Buffer{}

	go func() {
		for logM := range sub {
			select {
			case ch <- logM:
			default:
			}
		}
		close(ch)
	}()

	for logM := range ch {
		if logM.LogLevel < level {
			continue
		}
		buf.Reset()

		if err := json.NewEncoder(buf).Encode(Log{
			Type:    logM.Type(),
			Payload: logM.Payload,
		}); err != nil {
			break
		}

		var err error
		if wsConn == nil {
			_, err = w.Write(buf.Bytes())
			w.(http.Flusher).Flush()
		} else {
			err = wsConn.WriteMessage(websocket.TextMessage, buf.Bytes())
		}

		if err != nil {
			break
		}
	}
}

func version(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, r, render.M{"meta": C.Meta, "version": C.Version})
}
