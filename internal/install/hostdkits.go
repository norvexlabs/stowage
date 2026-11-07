package install

import (
	"bytes"
	"fmt"
	"net/http"
	"time"
)

// The host daemon's side of handoff kits, on the operators' socket alone: what a run gives includes
// credentials, as the provision hook's outputs do.
//
//	POST /v1/kits        KitRequest: the handoff's step run on the stack; IssuedKit, the kit sealed and
//	                     its code. Neither is kept: the answer is their only copy
//	POST /v1/kits/check  {handoff, instance}: the handoff's check run; {exit, log}

// KitCheckRequest is POST /v1/kits/check's body, and KitCheckResult its answer.
type KitCheckRequest struct {
	Handoff  string `json:"handoff"`
	Instance string `json:"instance"`
}

type KitCheckResult struct {
	Exit int    `json:"exit"`
	Log  string `json:"log,omitempty"`
}

func (h *Hostd) createKit(w http.ResponseWriter, r *http.Request) {
	var req KitRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	in, log := h.kitRun()
	if !h.kitReady(w, r, in) {
		return
	}
	issued, err := in.IssueKit(r.Context(), req, time.Now())
	if err != nil {
		replyErr(w, fmt.Errorf("%w\n%s", err, log.String()))
		return
	}
	issued.Log = log.String()
	reply(w, http.StatusOK, issued)
}

func (h *Hostd) checkKit(w http.ResponseWriter, r *http.Request) {
	var req KitCheckRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	in, log := h.kitRun()
	if !h.kitReady(w, r, in) {
		return
	}
	code, err := in.CheckKit(r.Context(), req.Handoff, req.Instance)
	if err != nil {
		replyErr(w, fmt.Errorf("%w\n%s", err, log.String()))
		return
	}
	reply(w, http.StatusOK, KitCheckResult{Exit: code, Log: log.String()})
}

// maxKitLog bounds what a step's stderr may add to the daemon's answer, which holds the kit's only copy
// and which the client reads up to a bound of its own.
const maxKitLog = 1 << 20

// boundedLog keeps the start of what is written, and swallows the rest.
type boundedLog struct {
	buf  bytes.Buffer
	over bool
}

func (b *boundedLog) Write(p []byte) (int, error) {
	if room := maxKitLog - b.buf.Len(); room < len(p) {
		b.over = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *boundedLog) String() string {
	if b.over {
		return b.buf.String() + fmt.Sprintf("\n… cut at %d bytes\n", maxKitLog)
	}
	return b.buf.String()
}

// kitRun is an installer whose output is a bounded log.
func (h *Hostd) kitRun() (*Installer, *boundedLog) {
	in, _ := h.run()
	log := &boundedLog{}
	in.O.Out, in.O.Log = log, log
	return in, log
}

// kitReady answers when no handoff can run yet: no install, a boot under way, or an install of another
// release than this OS's, whose stack its step was not written for.
func (h *Hostd) kitReady(w http.ResponseWriter, r *http.Request, in *Installer) bool {
	env, err := h.installed(in)
	if err == nil {
		err = h.booting(r.Context())
	}
	if err == nil {
		err = h.osRelease(env, "kit")
	}
	if err != nil {
		replyErr(w, err)
		return false
	}
	return true
}
