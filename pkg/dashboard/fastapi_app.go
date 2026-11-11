package dashboard

import (
	"context"
	"sync"
)

// Application of routes, middleware, state and lifespan, the FastAPI app over Starlette
type FastAPI struct {
	Title          string
	Version        string
	State          *State
	Router         *Router
	api            *APIRouter
	mu             sync.Mutex
	userMiddleware []Middleware
	stack          ASGIApp
}

// Application with a title, version and lifespan enter and exit hooks
func NewFastAPI(title, version string, enter, exit func(ctx context.Context) error) *FastAPI {
	app := &FastAPI{Title: title, Version: version, State: NewState(nil), api: NewAPIRouter("")}
	app.Router = &Router{RedirectSlash: true, lifespanEnter: enter, lifespanExit: exit}
	app.setup()
	return app
}

// Adds the documentation routes FastAPI serves by default
func (a *FastAPI) setup() {
	a.Router.Routes = append(a.Router.Routes, openAPIRoutes(a)...)
}

// Wraps the app with a middleware, outermost when added last, refused once the app has served
func (a *FastAPI) AddMiddleware(m Middleware) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stack != nil {
		return &RuntimeError{Msg: "Cannot add middleware after an application has started"}
	}
	a.userMiddleware = append([]Middleware{m}, a.userMiddleware...)
	return nil
}

// Adds an http dispatch middleware, the app.middleware("http") decorator
func (a *FastAPI) HTTPMiddleware(dispatch DispatchFunc) error {
	return a.AddMiddleware(BaseHTTPMiddleware(dispatch))
}

// Mounts the routes of a router under a prefix with extra dependencies
func (a *FastAPI) IncludeRouter(r *APIRouter, prefix string, deps ...*Dependency) {
	start := len(a.api.Routes)
	a.api.IncludeRouter(r, prefix, deps...)
	a.Router.Routes = append(a.Router.Routes, a.api.Routes[start:]...)
}

// Adds an http operation to the app router
func (a *FastAPI) AddAPIRoute(path string, methods []string, ep *Endpoint) {
	start := len(a.api.Routes)
	a.api.AddAPIRoute(path, methods, ep)
	a.Router.Routes = append(a.Router.Routes, a.api.Routes[start:]...)
}

// Adds a websocket operation to the app router
func (a *FastAPI) AddAPIWebSocketRoute(path string, ep *WSEndpoint) {
	start := len(a.api.Routes)
	a.api.AddAPIWebSocketRoute(path, ep)
	a.Router.Routes = append(a.Router.Routes, a.api.Routes[start:]...)
}

// Mounts an ASGI app below a path
func (a *FastAPI) Mount(path string, app ASGIApp, name string) {
	a.Router.Routes = append(a.Router.Routes, NewMount(path, app, name))
}

// Server error handler, user middleware, exception handlers and the router, outermost first
func (a *FastAPI) buildMiddlewareStack() ASGIApp {
	app := ASGIApp(a.Router.app)
	app = exceptionMiddleware(app)
	for i := len(a.userMiddleware) - 1; i >= 0; i-- {
		app = a.userMiddleware[i](app)
	}
	return serverErrorMiddleware(app)
}

// Serves one ASGI call, building the middleware stack on the first one
func (a *FastAPI) ASGI(ctx context.Context, scope *Scope, receive Receive, send Send) error {
	scope.App = a
	a.mu.Lock()
	if a.stack == nil {
		a.stack = a.buildMiddlewareStack()
	}
	stack := a.stack
	a.mu.Unlock()
	return stack(ctx, scope, receive, send)
}
