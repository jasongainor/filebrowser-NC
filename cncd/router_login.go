package cncd

import "github.com/gorilla/mux"

// registerLogin mounts POST /api/login, POST /api/logout, and
// GET /api/me on r (the "/api" subrouter NewRouter already built) and
// publishes the loginService it constructs as the process's active
// one, so authzFor (auth.go) can validate session cookies on every
// other route without Deps growing a field just for this. See
// login.go's package doc comment and the "Active login service
// registry" section for why.
//
// Split into its own file, called from NewRouter with a single line,
// so a concurrent change to router.go's other routes only conflicts
// on that one line.
func registerLogin(r *mux.Router, d Deps) {
	svc := newLoginService(d)
	setActiveLogin(svc)

	r.HandleFunc("/login", svc.handleLogin).Methods("POST")
	r.HandleFunc("/logout", svc.handleLogout).Methods("POST")
	r.HandleFunc("/me", svc.handleMe).Methods("GET")
}
