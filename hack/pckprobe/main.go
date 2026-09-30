// pckprobe executes the prompt-cache-policy assertions as a plain program (fleet hosts deny
// the Go unit runner; this is NOT a substitute for the CI run of the unit suites, it is the narrow
// check the wire proof needs before an enforce window). Run: go run ./hack/pckprobe
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var fails int

func check(name string, ok bool, detail ...any) {
	if ok {
		fmt.Println("PASS", name)
		return
	}
	fails++
	fmt.Println("FAIL", name, fmt.Sprint(detail...))
}

func body(sys, user string) []byte {
	return []byte(`{"model":"gpt-6-sol","messages":[{"role":"system","content":"` + sys + `"},{"role":"user","content":"` + user + `"}]}`)
}

func main() {
	// resolver: distinct prefixes -> distinct derived keys; same prefix -> same key
	r1 := cliproxyexecutor.ResolvePromptCacheKey("codex", body("S1", "u"), nil, nil)
	r1b := cliproxyexecutor.ResolvePromptCacheKey("codex", body("S1", "u"), nil, nil)
	r2 := cliproxyexecutor.ResolvePromptCacheKey("codex", body("S2", "u"), nil, nil)
	check("derived source", r1.Source == "derived" && r2.Source == "derived", r1, r2)
	check("derived deterministic", r1.Key == r1b.Key && r1.ID == r1b.ID)
	check("derived distinct per prefix", r1.Key != r2.Key && r1.ID != r2.ID)
	// caller / native / session / passthrough
	rc := cliproxyexecutor.ResolvePromptCacheKey("codex", []byte(`{"model":"m","prompt_cache_key":"fr-1","messages":[{"role":"user","content":"u"}]}`), nil, nil)
	check("caller verbatim", rc.Source == "caller" && rc.Key == "fr-1", rc)
	rn := cliproxyexecutor.ResolvePromptCacheKey("codex", body("S1", "u"), http.Header{"Session_id": {"native-1"}}, nil)
	check("native header verbatim caller", rn.Source == "caller" && rn.Key == "native-1", rn)
	rs := cliproxyexecutor.ResolvePromptCacheKey("codex", body("S1", "u"), http.Header{"X-Session-Id": {"xs-1"}}, nil)
	check("x-session-id hashed session", rs.Source == "session" && strings.HasPrefix(rs.Key, "pck-") && !strings.Contains(rs.Key, "xs-1"), rs)
	h := http.Header{"X-Session-Id": {"xs-1"}}
	h.Set(cliproxyexecutor.PromptCachePolicyHeader, "passthrough")
	rp := cliproxyexecutor.ResolvePromptCacheKey("codex", []byte(`{"model":"m","prompt_cache_key":"fr-1","messages":[{"role":"user","content":"u"}]}`), h, nil)
	check("passthrough beats caller+session", rp.Source == "passthrough" && rp.Key == "", rp)
	// mode
	check("mode default shadow", cliproxyexecutor.NormalizePromptCacheKeyMode("") == "shadow" && cliproxyexecutor.NormalizePromptCacheKeyMode("enforced") == "shadow" && cliproxyexecutor.NormalizePromptCacheKeyMode(" ENFORCE ") == "enforce")
	check("mode unknown reported", cliproxyexecutor.PromptCacheKeyModeUnknown("bogus") && !cliproxyexecutor.PromptCacheKeyModeUnknown("") && !cliproxyexecutor.PromptCacheKeyModeUnknown("shadow"))
	mShadow := cliproxyexecutor.ApplyPromptCacheKeyMetadata(nil, r1, "shadow")
	check("shadow records no wire key", cliproxyexecutor.WirePromptCacheKeyFromMetadata(mShadow) == "" && cliproxyexecutor.PromptCacheKeySourceFromMetadata(mShadow) == "derived" && cliproxyexecutor.PromptCacheKeyIDFromMetadata(mShadow) == r1.ID)
	mEnf := cliproxyexecutor.ApplyPromptCacheKeyMetadata(nil, r1, "enforce")
	check("enforce records wire key", cliproxyexecutor.WirePromptCacheKeyFromMetadata(mEnf) == r1.Key)
	mCaller := cliproxyexecutor.ApplyPromptCacheKeyMetadata(nil, rc, "enforce")
	check("enforce records caller key for affinity", cliproxyexecutor.WirePromptCacheKeyFromMetadata(mCaller) == "fr-1")

	// selector: rule 0 pins on the recorded wire key; distinct keys -> distinct bindings (round-robin)
	sel := cliproxyauth.NewSessionAffinitySelector(&cliproxyauth.RoundRobinSelector{})
	auths := []*cliproxyauth.Auth{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	opts := func(res cliproxyexecutor.PromptCacheKeyResolution, mode string, payload []byte) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{OriginalRequest: payload, Metadata: cliproxyexecutor.ApplyPromptCacheKeyMetadata(nil, res, mode)}
	}
	first, err := sel.Pick(context.Background(), "codex", "gpt-6-sol", opts(r1, "enforce", body("S1", "u")), auths)
	check("pick ok", err == nil && first != nil, err)
	stable := true
	for i := 0; i < 5; i++ {
		got, _ := sel.Pick(context.Background(), "codex", "gpt-6-sol", opts(r1, "enforce", body("S1", "u"+fmt.Sprint(i))), auths)
		stable = stable && got.ID == first.ID
	}
	check("same derived key -> same auth over 5 legs (later turns differ)", stable)
	other, _ := sel.Pick(context.Background(), "codex", "gpt-6-sol", opts(r2, "enforce", body("S2", "u")), auths)
	check("distinct derived key -> distinct binding", other.ID != first.ID, first.ID, other.ID)
	e1 := cliproxyauth.ExtractSessionID(nil, body("S1", "u"), mEnf)
	e2 := cliproxyauth.ExtractSessionID(nil, body("S2", "u"), cliproxyexecutor.ApplyPromptCacheKeyMetadata(nil, r2, "enforce"))
	check("extracted affinity id = derived:<wire key>", e1 == "derived:"+r1.Key && e2 == "derived:"+r2.Key && e1 != e2, e1, e2)
	eS := cliproxyauth.ExtractSessionID(nil, body("S1", "u"), mShadow)
	check("shadow falls back to native extraction", strings.HasPrefix(eS, "msg:"), eS)

	// bounded cache (v8: upstream LRU capacity, 5679bbf3)
	c := cliproxyauth.NewSessionCacheWithCapacity(time.Hour, 3)
	defer c.Stop()
	c.Set("s1", "a")
	c.Set("s2", "b")
	c.Set("s3", "c")
	c.GetAndRefresh("s1")
	c.Set("s4", "d")
	_, s2ok := c.Get("s2")
	check("cache cap evicts closest to expiry", c.Len() == 3 && !s2ok, c.Len(), s2ok)

	if fails > 0 {
		fmt.Println("FAILURES:", fails)
		os.Exit(1)
	}
	fmt.Println("ALL PASS")
}
