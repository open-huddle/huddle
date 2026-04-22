package auth_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"github.com/open-huddle/huddle/apps/api/internal/auth"
	huddlev1 "github.com/open-huddle/huddle/gen/go/huddle/v1"
)

// fakeVerifier implements auth.TokenVerifier without a real OIDC provider.
// It either returns canned claims or a canned error based on the exact raw
// token passed in, so tests can script the success and failure paths
// independently of token parsing.
type fakeVerifier struct {
	claimsFor map[string]auth.Claims
	errFor    map[string]error
}

func (f *fakeVerifier) Verify(_ context.Context, raw string) (auth.Claims, error) {
	if err, ok := f.errFor[raw]; ok {
		return auth.Claims{}, err
	}
	if c, ok := f.claimsFor[raw]; ok {
		return c, nil
	}
	return auth.Claims{}, errors.New("fake verifier: unexpected token")
}

// newRequest builds a minimal connect.Request around a throwaway proto
// message. The interceptor only reads Header() so the payload shape is
// irrelevant — but we need *some* connect-registered type to satisfy
// connect.NewRequest's generic constraint.
func newRequest(authHeader string) *connect.Request[huddlev1.MessageServiceListRequest] {
	req := connect.NewRequest(&huddlev1.MessageServiceListRequest{})
	if authHeader != "" {
		req.Header().Set("Authorization", authHeader)
	}
	return req
}

// --- WrapUnary -------------------------------------------------------------

func TestWrapUnary_ValidToken_AttachesClaims(t *testing.T) {
	t.Parallel()

	want := auth.Claims{Subject: "alice", Email: "alice@example.test", Name: "Alice"}
	v := &fakeVerifier{claimsFor: map[string]auth.Claims{"good-token": want}}
	interceptor := auth.NewInterceptor(v)

	var seen auth.Claims
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		got, ok := auth.ClaimsFrom(ctx)
		if !ok {
			t.Fatal("handler reached without claims on context")
		}
		seen = got
		return connect.NewResponse(&huddlev1.MessageServiceListResponse{}), nil
	})

	resp, err := interceptor.WrapUnary(next)(context.Background(), newRequest("Bearer good-token"))
	if err != nil {
		t.Fatalf("WrapUnary: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if seen != want {
		t.Errorf("claims on context: want %+v got %+v", want, seen)
	}
}

func TestWrapUnary_MissingHeader_Unauthenticated(t *testing.T) {
	t.Parallel()

	v := &fakeVerifier{}
	interceptor := auth.NewInterceptor(v)
	next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		t.Fatal("next should not be called when the Authorization header is missing")
		return nil, nil
	})

	_, err := interceptor.WrapUnary(next)(context.Background(), newRequest(""))
	assertUnauthenticated(t, err)
}

func TestWrapUnary_MalformedBearerPrefix_Unauthenticated(t *testing.T) {
	t.Parallel()

	v := &fakeVerifier{}
	interceptor := auth.NewInterceptor(v)
	next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		t.Fatal("next should not be called when the Authorization header is malformed")
		return nil, nil
	})

	for _, h := range []string{
		"Token abc",          // wrong scheme
		"bearer",             // prefix only, no token
		"Bearer",             // missing trailing space + token
		"Basic dXNlcjpwYXNz", // different scheme entirely
	} {
		_, err := interceptor.WrapUnary(next)(context.Background(), newRequest(h))
		assertUnauthenticated(t, err)
	}
}

func TestWrapUnary_VerifierError_Unauthenticated(t *testing.T) {
	t.Parallel()

	v := &fakeVerifier{errFor: map[string]error{"expired-token": errors.New("token expired")}}
	interceptor := auth.NewInterceptor(v)
	next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		t.Fatal("next should not be called when the verifier rejects the token")
		return nil, nil
	})

	_, err := interceptor.WrapUnary(next)(context.Background(), newRequest("Bearer expired-token"))
	assertUnauthenticated(t, err)
}

// --- WrapStreamingHandler --------------------------------------------------

// fakeStream is a minimal connect.StreamingHandlerConn implementation. The
// interceptor only reads RequestHeader() before handing the conn to next —
// everything else is pass-through and irrelevant to these tests.
type fakeStream struct {
	reqHeader http.Header
}

func (f *fakeStream) Spec() connect.Spec { return connect.Spec{} }
func (f *fakeStream) Peer() connect.Peer { return connect.Peer{} }
func (f *fakeStream) Receive(any) error  { return nil }
func (f *fakeStream) RequestHeader() http.Header {
	if f.reqHeader == nil {
		f.reqHeader = http.Header{}
	}
	return f.reqHeader
}
func (f *fakeStream) Send(any) error               { return nil }
func (f *fakeStream) ResponseHeader() http.Header  { return http.Header{} }
func (f *fakeStream) ResponseTrailer() http.Header { return http.Header{} }

func newStream(authHeader string) *fakeStream {
	s := &fakeStream{reqHeader: http.Header{}}
	if authHeader != "" {
		s.reqHeader.Set("Authorization", authHeader)
	}
	return s
}

func TestWrapStreamingHandler_ValidToken_AttachesClaims(t *testing.T) {
	t.Parallel()

	want := auth.Claims{Subject: "alice", Email: "alice@example.test", Name: "Alice"}
	v := &fakeVerifier{claimsFor: map[string]auth.Claims{"good-token": want}}
	interceptor := auth.NewInterceptor(v)

	var seen auth.Claims
	next := connect.StreamingHandlerFunc(func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		got, ok := auth.ClaimsFrom(ctx)
		if !ok {
			t.Fatal("streaming handler reached without claims on context")
		}
		seen = got
		return nil
	})

	if err := interceptor.WrapStreamingHandler(next)(context.Background(), newStream("Bearer good-token")); err != nil {
		t.Fatalf("WrapStreamingHandler: %v", err)
	}
	if seen != want {
		t.Errorf("claims on context: want %+v got %+v", want, seen)
	}
}

// Regression guard for the bug captured in the feedback memory: Connect's
// UnaryInterceptorFunc silently skips streaming handlers, so an interceptor
// that "seems to work" might be leaving streams unauthenticated. This test
// fails if the streaming path is not wrapped at all.
func TestWrapStreamingHandler_MissingHeader_Unauthenticated(t *testing.T) {
	t.Parallel()

	v := &fakeVerifier{}
	interceptor := auth.NewInterceptor(v)
	next := connect.StreamingHandlerFunc(func(context.Context, connect.StreamingHandlerConn) error {
		t.Fatal("streaming next should not be called when the Authorization header is missing")
		return nil
	})

	err := interceptor.WrapStreamingHandler(next)(context.Background(), newStream(""))
	assertUnauthenticated(t, err)
}

func TestWrapStreamingHandler_VerifierError_Unauthenticated(t *testing.T) {
	t.Parallel()

	v := &fakeVerifier{errFor: map[string]error{"expired-token": errors.New("token expired")}}
	interceptor := auth.NewInterceptor(v)
	next := connect.StreamingHandlerFunc(func(context.Context, connect.StreamingHandlerConn) error {
		t.Fatal("streaming next should not be called when the verifier rejects the token")
		return nil
	})

	err := interceptor.WrapStreamingHandler(next)(context.Background(), newStream("Bearer expired-token"))
	assertUnauthenticated(t, err)
}

// --- WrapStreamingClient ---------------------------------------------------

func TestWrapStreamingClient_IsPassThrough(t *testing.T) {
	t.Parallel()

	v := &fakeVerifier{}
	interceptor := auth.NewInterceptor(v)

	called := false
	next := connect.StreamingClientFunc(func(_ context.Context, _ connect.Spec) connect.StreamingClientConn {
		called = true
		return nil
	})

	wrapped := interceptor.WrapStreamingClient(next)
	_ = wrapped(context.Background(), connect.Spec{})
	if !called {
		t.Error("WrapStreamingClient must be a pass-through — this process is a server, not an outbound streaming client")
	}
}

// --- helpers ---------------------------------------------------------------

func assertUnauthenticated(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want *connect.Error, got %T: %v", err, err)
	}
	if got := ce.Code(); got != connect.CodeUnauthenticated {
		t.Errorf("code: want Unauthenticated, got %v", got)
	}
}
