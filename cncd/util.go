package cncd

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// renderJSON writes v as the JSON response body. Mirrors fbhttp's
// renderJSON (http/utils.go) so response shapes match byte for byte.
func renderJSON(w http.ResponseWriter, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, err = w.Write(buf)
	return err
}

// writeError writes a plain-text status + message body, matching
// fbhttp's handle() wrapper (http/data.go): "<code> <text> (<err>)"
// for 400s with an error, "<code> <text>" otherwise.
func writeError(w http.ResponseWriter, status int, err error) {
	txt := http.StatusText(status)
	if status == http.StatusBadRequest && err != nil {
		txt += " (" + err.Error() + ")"
	}
	http.Error(w, strconv.Itoa(status)+" "+txt, status)
}
