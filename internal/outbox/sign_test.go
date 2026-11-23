package outbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

// signedHook keeps each post's body and signature header.
type signedHook struct {
	mu    sync.Mutex
	posts [][2]string
}

func (h *signedHook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.posts = append(h.posts, [2]string{string(b), r.Header.Get(notice.SignatureHeader)})
	h.mu.Unlock()
	w.WriteHeader(200)
}

func TestWebhookPostsAreSignedAndSecretsRotate(t *testing.T) {
	h := &signedHook{}
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	o, _, now := setup(t, nil)
	o.UseClient(srv.Client())
	ctx := context.Background()
	ch, first, err := o.AddChannel(ctx, "board", notice.Webhook, srv.URL, "admin@example.com")
	if err != nil || first == "" || !ch.Signs() {
		t.Fatalf("add %+v %q %v", ch, first, err)
	}
	if _, secret, _ := o.AddChannel(ctx, "teams", notice.Teams, srv.URL+"/t", "a"); secret != "" {
		t.Error("only a webhook channel gets a secret")
	}
	if _, err := o.Test(ctx, ch.ID, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	body, header := h.posts[0][0], h.posts[0][1]
	if !notice.Verify(first, header, []byte(body), *now, 5*time.Minute) {
		t.Fatalf("the post does not verify: %s", header)
	}
	if notice.Verify("whsec_wrong", header, []byte(body), *now, 5*time.Minute) || notice.Verify(first, header, []byte(body+" "), *now, 5*time.Minute) {
		t.Error("verified with the wrong secret or body")
	}
	if notice.Verify(first, header, []byte(body), now.Add(time.Hour), 5*time.Minute) {
		t.Error("an old post verified")
	}
	second, err := o.RotateSecret(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	o.Test(ctx, ch.ID, "admin@example.com")
	body, header = h.posts[1][0], h.posts[1][1]
	if !notice.Verify(first, header, []byte(body), *now, time.Minute) || !notice.Verify(second, header, []byte(body), *now, time.Minute) {
		t.Errorf("while rotating, both secrets verify: %s", header)
	}
	o.DropOldSecret(ctx, ch.ID)
	*now = now.Add(time.Second)
	o.Test(ctx, ch.ID, "admin@example.com")
	body, header = h.posts[2][0], h.posts[2][1]
	if notice.Verify(first, header, []byte(body), *now, time.Minute) || !notice.Verify(second, header, []byte(body), *now, time.Minute) {
		t.Errorf("after dropping, only the new one: %s", header)
	}
}
