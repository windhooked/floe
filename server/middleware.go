package server

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"

	"github.com/floeit/floe/hub"
	"github.com/floeit/floe/log"
)

const (
	rOK       = http.StatusOK
	rUnauth   = http.StatusUnauthorized
	rBad      = http.StatusBadRequest
	rNotFound = http.StatusNotFound
	rErr      = http.StatusInternalServerError
	rCreated  = http.StatusCreated
	rConflict = http.StatusConflict

	cookieName = "floe-sesh"
)

// AdminToken a configurable admin token for this host
var AdminToken string

type renderable interface{}

func decodeBody(rw http.ResponseWriter, r *http.Request, v interface{}) (bool, int, string) {
	defer r.Body.Close()

	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return false, rBad, err.Error()
	}

	return true, 0, ""
}

func jsonResp(w http.ResponseWriter, code int, r interface{}) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		log.Debug(err)
		log.Debugf("%#v", r)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"Status": "Fail", "Payload": "` + err.Error() + `"}`))
		return
	}

	w.WriteHeader(code)
	w.Write(b)
}

type context struct {
	ps   *httprouter.Params
	sesh *session
	hub  *hub.Hub
}

type notFoundHandler struct{}

func (h notFoundHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	jsonResp(rw, rNotFound, wrapper{Message: "not found"})
}

type contextFunc func(rw http.ResponseWriter, r *http.Request, ctx *context) (int, string, renderable)

type wrapper struct {
	Message string
	Payload renderable
}

type handler struct {
	hub *hub.Hub
}

// the boolean return is set true if the caller should produce its own response
func authRequest(rw http.ResponseWriter, r *http.Request) *session {
	var code int
	var sesh *session

	tok := r.Header.Get("X-Floe-Auth")
	if tok == "" {
		log.Debug("checking cookie")
		c, err := r.Cookie(cookieName)
		if err != nil {
			log.Warning("cookie problem", err)
		} else {
			tok = c.Value
		}
	}

	if tok == "" {
		code = rUnauth
		jsonResp(rw, code, wrapper{Message: "missing session"})
		return nil
	}

	log.Debug("checking token", tok, AdminToken)

	// default to this agent for testing admin token
	if tok == AdminToken {
		log.Debug("found admin token", tok)
		sesh = &session{
			token:      tok,
			lastActive: time.Now(),
			user:       "Admin",
		}
	}

	if sesh == nil {
		sesh = goodToken(tok)
		if sesh == nil {
			code = rUnauth
			jsonResp(rw, code, wrapper{Message: "invalid session"})
			return nil
		}
	}

	// refresh cookie
	setCookie(rw, tok)

	return sesh
}

func (h handler) mw(f contextFunc, auth bool) func(rw http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	fn := func(rw http.ResponseWriter, r *http.Request, ps httprouter.Params) {

		var code int
		start := time.Now()
		log.Debugf("req: %s %s", r.Method, r.URL.String())
		defer func() {
			log.Debugf("rsp: %v %s %d %s", time.Since(start), r.Method, code, r.URL.String())
		}()

		cors(rw, r)

		// handler nil is the options catch all so the cors response above is all we need
		if f == nil {
			code = rOK
			jsonResp(rw, code, "ok")
			return
		}

		// authenticate session is needed
		var sesh *session
		if auth {
			sesh = authRequest(rw, r)
			if sesh == nil {
				return
			}
		}

		// got here then we are authenticated - so call the specific handler
		ctx := &context{
			ps:   &ps,
			sesh: sesh,
			hub:  h.hub,
		}

		code, msg, res := f(rw, r, ctx)
		// code 0 means the function responded itself
		if code == 0 {
			return
		}

		if msg == "" && code == rOK {
			msg = "OK"
		}
		reply := wrapper{
			Message: msg,
			Payload: res,
		}

		jsonResp(rw, code, reply)
	}

	return zipper(fn)
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (w gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

func zipper(fn func(rw http.ResponseWriter, r *http.Request, ps httprouter.Params)) func(rw http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	return func(rw http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			fn(rw, r, ps)
			return
		}
		rw.Header().Set("Vary", "Accept-Encoding")
		rw.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(rw)
		defer gz.Close()
		gzr := gzipResponseWriter{Writer: gz, ResponseWriter: rw}
		fn(gzr, r, ps)
	}
}

func serveFiles(r *httprouter.Router, path string, root http.FileSystem) {
	if len(path) < 10 || path[len(path)-10:] != "/*filepath" {
		panic("path must end with /*filepath in path '" + path + "'")
	}

	fileServer := http.FileServer(root)

	r.GET(path, zipper(func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		req.URL.Path = ps.ByName("filepath")
		fileServer.ServeHTTP(w, req)
	}))
}

// setupTriggers goes through all the known trigger types to set up the associated routes
func (h handler) setupPushes(basePath string, r *httprouter.Router, hub *hub.Hub) {
	for subPath, t := range pushes {

		authenticated := t.RequiresAuth()

		// TODO consider parameterised paths
		g := t.GetHandler(hub.Queue())
		if g != nil {
			r.GET(basePath+subPath, h.mw(adaptSub(hub, g), authenticated))
		}
		p := t.PostHandler(hub.Queue())
		if p != nil {
			r.POST(basePath+subPath, h.mw(adaptSub(hub, p), authenticated))
		}
	}
}

func adaptSub(hub *hub.Hub, handle httprouter.Handle) contextFunc {
	return func(w http.ResponseWriter, req *http.Request, ctx *context) (int, string, renderable) {
		handle(w, req, *ctx.ps)
		return 0, "", nil // each subscriber handler is responsible for the response
	}
}

func setCookie(rw http.ResponseWriter, tok string) {
	expiration := time.Now().Add(seshLifetime)
	cookie := http.Cookie{Name: cookieName, Value: tok, Expires: expiration, Path: "/"}
	http.SetCookie(rw, &cookie)
}

func cors(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Access-Control-Allow-Origin", "*")
	rw.Header().Set("Access-Control-Allow-Methods", "POST, GET, PUT, OPTIONS, DELETE")
	rw.Header().Set("Access-Control-Allow-Headers", strings.Join(r.Header["Access-Control-Request-Headers"], ","))
}

func panicHandler(rw http.ResponseWriter, r *http.Request, v interface{}) {
	log.Error("PANIC in ", r.URL.String())
	log.Error(v)

	stack := debug.Stack()

	jsonResp(rw, http.StatusInternalServerError, string(stack))

	// send it to stderr
	fmt.Fprintf(os.Stderr, string(stack))
	// this sends it to the client....
	// fmt.Fprintf(rw, f, err, )
}
