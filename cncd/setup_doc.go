package cncd

// The setup sheet the post writes beside a program, and the QR code on the
// display that opens it. The post puts it in a folder named like the .nc
// (3202.nc -> 3202/3202.html, with the sidecar json beside it); the
// display's list pages carry a QR of /setup/<program>, which redirects to
// that file. Read-only and unauthenticated, like the display fetch: cncd is
// LAN and tailnet only.

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/mux"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/filebrowser/filebrowser/v2/cnc"
)

func registerSetupDoc(r *mux.Router, d Deps) {
	r.PathPrefix("/setup/{program}").HandlerFunc(d.setupDocHandler).Methods("GET", "HEAD")
}

// setupDocHandler: /setup/3202 redirects to the setup sheet in the
// program's folder; /setup/3202/<file> serves files from that folder (so
// the sheet's own images and links resolve).
func (d Deps) setupDocHandler(w http.ResponseWriter, r *http.Request) {
	program := cnc.LoadedProgramNumber(mux.Vars(r)["program"])
	pf, err := cnc.FindProgramFile(d.Root, program)
	if err != nil || pf == nil || pf.Folder == "" {
		http.Error(w, fmt.Sprintf("no folder for program %s", program), http.StatusNotFound)
		return
	}
	prefix := "/setup/" + mux.Vars(r)["program"]
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == "" || rest == "/" {
		if pf.Doc == "" {
			http.Error(w, fmt.Sprintf("no setup sheet in the %s folder", program), http.StatusNotFound)
			return
		}
		http.Redirect(w, r, prefix+"/"+url.PathEscape(pf.Doc), http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.StripPrefix(prefix, http.FileServer(http.Dir(pf.Folder))).ServeHTTP(w, r)
}

// setupQR is the QR for the loaded program's setup sheet, nil when the
// program has no sheet. host is the address the display reached cncd on,
// so the phone scanning it uses the same one.
func (d Deps) setupQR(program, host string) *cnc.SheetQR {
	pf, err := cnc.FindProgramFile(d.Root, program)
	if err != nil || pf == nil || pf.Doc == "" || host == "" {
		return nil
	}
	link := "http://" + host + "/setup/" + program
	q, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		return nil
	}
	q.DisableBorder = true
	return &cnc.SheetQR{URL: link, Rows: qrRows(q.Bitmap())}
}

// qrRows packs a module matrix into one hex string per row, most
// significant bit first, padded to whole nibbles. '1' bits are dark.
func qrRows(bm [][]bool) []string {
	rows := make([]string, len(bm))
	for i, row := range bm {
		var b strings.Builder
		for x := 0; x < len(row); x += 4 {
			n := 0
			for k := 0; k < 4; k++ {
				n <<= 1
				if x+k < len(row) && row[x+k] {
					n |= 1
				}
			}
			b.WriteByte("0123456789abcdef"[n])
		}
		rows[i] = b.String()
	}
	return rows
}
