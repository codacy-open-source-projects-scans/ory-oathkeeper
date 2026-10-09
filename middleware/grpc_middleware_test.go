// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package middleware_test

//go:generate go tool mockgen -destination=grpc_mock_server_test.go -package=middleware_test github.com/ory/rpctest/gen/go/ory/rpctest TestServiceServer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/ory/rpctest/gen/go/ory/rpctest"

	"github.com/ory/oathkeeper/driver/configuration"
	"github.com/ory/oathkeeper/middleware"
	"github.com/ory/oathkeeper/rule"
)

func testClient(t *testing.T, ts *httptest.Server, bearer *string) rpctest.TestServiceClient {
	certs := x509.NewCertPool()
	certs.AddCert(ts.Certificate())
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: certs})),
		grpc.WithAuthority("myproject.example.com"),
	}
	if bearer != nil {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(testToken(*bearer)))
	}
	conn, err := grpc.Dial(ts.Listener.Addr().String(), dialOpts...) //nolint:staticcheck // grpc.Dial is adequate for tests in grpc v1.x
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return rpctest.NewTestServiceClient(conn)
}

func testTokenCheckServer(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("authorization") != "Bearer correct token" {
				t.Logf("denied request %+v", r)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			t.Logf("allowed request %+v", r)
			_, _ = io.WriteString(w, "{}")
		}))
	t.Cleanup(s.Close)
	return s
}

type testToken string

func (t testToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}
func (t testToken) RequireTransportSecurity() bool { return false }

type upstream struct {
	*MockTestServiceServer
	rpctest.UnsafeTestServiceServer
}

func TestGRPCMiddleware(t *testing.T) {
	tokenCheckServer := testTokenCheckServer(t)

	ctrl := gomock.NewController(t)
	upstream := upstream{MockTestServiceServer: NewMockTestServiceServer(ctrl)}

	config := writeTestConfigf(t, `
authenticators:
  noop:
    enabled: true
  anonymous:
    enabled: true
  bearer_token:
    enabled: true
    config:
      check_session_url: %s
authorizers:
  allow:
    enabled: true
  deny:
    enabled: true
mutators:
  noop:
    enabled: true
`, tokenCheckServer.URL)

	mw, err := middleware.New(t.Context(), middleware.WithConfigFile(config))
	require.NoError(t, err)
	reg := mw.Registry()

	s := grpc.NewServer(
		grpc.UnaryInterceptor(mw.UnaryInterceptor()),
		grpc.StreamInterceptor(mw.StreamInterceptor()),
	)
	rpctest.RegisterTestServiceServer(s, upstream)
	ts := httptest.NewUnstartedServer(s)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)

	upstream.EXPECT().
		Unary(gomock.Any(), gomock.Any()).
		AnyTimes().
		Return(&rpctest.UnaryResponse{}, nil)

	cases := []struct {
		name   string
		rules  map[configuration.MatchingStrategy][]rule.Rule
		bearer *string
		assert assert.ErrorAssertionFunc
	}{
		{
			name: "should deny on empty ruleset",
			rules: map[configuration.MatchingStrategy][]rule.Rule{
				configuration.Regexp: {},
				configuration.Glob:   {},
			},
			assert: assertErrDenied,
		},
		{
			name: "should succeed on matching rule",
			rules: map[configuration.MatchingStrategy][]rule.Rule{
				configuration.Regexp: {{
					Match: &rule.MatchRPC{
						Authority:  "<([0-9a-zA-Z\\-]+)>.example.com",
						FullMethod: "ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "anonymous"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
				configuration.Glob: {{
					Match: &rule.MatchRPC{
						Authority:  "<**>.example.com",
						FullMethod: "ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "anonymous"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
			},
			assert: assert.NoError,
		},
		{
			name: "should block on matching rule when authorizer denies",
			rules: map[configuration.MatchingStrategy][]rule.Rule{
				configuration.Regexp: {{
					Match: &rule.MatchRPC{
						Authority:  "<([0-9a-zA-Z\\-]+)>.example.com",
						FullMethod: "ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "anonymous"}},
					Authorizer:     rule.Handler{Handler: "deny"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
				configuration.Glob: {{
					Match: &rule.MatchRPC{
						Authority:  "<**>.example.com",
						FullMethod: "ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "anonymous"}},
					Authorizer:     rule.Handler{Handler: "deny"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
			},
			assert: assertErrDenied,
		},
		{
			name: "should succeed on matching rule with wildcard",
			rules: map[configuration.MatchingStrategy][]rule.Rule{
				configuration.Regexp: {{
					Match: &rule.MatchRPC{
						Authority:  "<.*>",
						FullMethod: "<.*>U<.*>",
					},
					Authenticators: []rule.Handler{{Handler: "anonymous"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
				configuration.Glob: {{
					Match: &rule.MatchRPC{
						Authority:  "<**>.example.com",
						FullMethod: "ory.rpctest.TestService/U<**>",
					},
					Authenticators: []rule.Handler{{Handler: "anonymous"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
			},
			assert: assert.NoError,
		},
		{
			name: "should fail on mis-matching rule with wildcard",
			rules: map[configuration.MatchingStrategy][]rule.Rule{
				configuration.Regexp: {{
					Match: &rule.MatchRPC{
						Authority:  "<[0-9a-zA-Z]+>.example.com",
						FullMethod: "ory.rpctest.TestService/U<[a-zA-Z]+>-mismatch",
					},
					Authenticators: []rule.Handler{{Handler: "anonymous"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
				configuration.Glob: {{
					Match: &rule.MatchRPC{
						Authority:  "<**>.example.com",
						FullMethod: "ory.rpctest.TestService/U<**>-mismatch",
					},
					Authenticators: []rule.Handler{{Handler: "anonymous"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
			},
			assert: assertErrDenied,
		},
		{
			name: "should fail because not an rpc rule",
			rules: map[configuration.MatchingStrategy][]rule.Rule{
				configuration.Regexp: {{
					Match: &rule.Match{
						Methods: []string{"POST"},
						URL:     "rpc://<[0-9a-zA-Z]+>.example.com/ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "anonymous"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
				configuration.Glob: {{
					Match: &rule.Match{
						Methods: []string{"POST"},
						URL:     "rpc://<**>.example.com/ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "anonymous"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
			},
			assert: assertErrDenied,
		},
		{
			name: "should fail when no bearer token is supplied",
			rules: map[configuration.MatchingStrategy][]rule.Rule{
				configuration.Regexp: {{
					Match: &rule.MatchRPC{
						Authority:  "<([0-9a-zA-Z\\-]+)>.example.com",
						FullMethod: "ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "bearer_token"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
				configuration.Glob: {{
					Match: &rule.MatchRPC{
						Authority:  "<**>.example.com",
						FullMethod: "ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "bearer_token"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
			},
			assert: assertErrDenied,
		},
		{
			name: "should succeed when correct bearer token is supplied",
			rules: map[configuration.MatchingStrategy][]rule.Rule{
				configuration.Regexp: {{
					Match: &rule.MatchRPC{
						Authority:  "<([0-9a-zA-Z\\-]+)>.example.com",
						FullMethod: "ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "bearer_token"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
				configuration.Glob: {{
					Match: &rule.MatchRPC{
						Authority:  "<**>.example.com",
						FullMethod: "ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "bearer_token"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
			},
			bearer: new("correct token"),
			assert: assert.NoError,
		},
		{
			name: "should fail when incorrect bearer token is supplied",
			rules: map[configuration.MatchingStrategy][]rule.Rule{
				configuration.Regexp: {{
					Match: &rule.MatchRPC{
						Authority:  "<([0-9a-zA-Z\\-]+)>.example.com",
						FullMethod: "ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "bearer_token"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
				configuration.Glob: {{
					Match: &rule.MatchRPC{
						Authority:  "<**>.example.com",
						FullMethod: "ory.rpctest.TestService/Unary",
					},
					Authenticators: []rule.Handler{{Handler: "bearer_token"}},
					Authorizer:     rule.Handler{Handler: "allow"},
					Mutators:       []rule.Handler{{Handler: "noop"}},
				}},
			},
			bearer: new("incorrect token"),
			assert: assertErrDenied,
		},
	}

	strategies := []configuration.MatchingStrategy{configuration.Regexp, configuration.Glob}

	for _, tc := range cases {
		t.Run("case="+tc.name, func(t *testing.T) {
			client := testClient(t, ts, tc.bearer)
			for _, s := range strategies {
				t.Run("strategy="+string(s), func(t *testing.T) {
					require.NoError(t, reg.RuleRepository().SetMatchingStrategy(t.Context(), s))
					require.NoError(t, reg.RuleRepository().Set(t.Context(), tc.rules[s]))

					_, err := client.Unary(t.Context(), &rpctest.UnaryRequest{})
					tc.assert(t, err)

					srvStream, err := client.ServerStream(t.Context(), &rpctest.ServerStreamRequest{})
					require.NoError(t, err)
					_, err = srvStream.Recv()
					assertErrDenied(t, err)

					clStream, err := client.ClientStream(t.Context())
					require.NoError(t, err)
					_, err = clStream.CloseAndRecv()
					assertErrDenied(t, err)

					bidiStream, err := client.BidiStream(t.Context())
					require.NoError(t, err)
					_, err = bidiStream.Recv()
					assertErrDenied(t, err)
				})
			}
		})
	}
}

func assertErrDenied(t assert.TestingT, err error, _ ...any) bool {
	r := require.New(t.(require.TestingT))
	r.Error(err)

	sErr, sOK := status.FromError(err)
	cErr, cOK := errors.AsType[*connect.Error](err)
	r.Truef(sOK || cOK, "error %v is neither *status.Status nor *connect.Error (type: %[1]T)", err)

	code := 0
	if sOK {
		code = int(sErr.Code())
	} else if cOK {
		code = int(cErr.Code())
	}

	return assert.EqualValuesf(t, codes.Unauthenticated, code, "%+v", err)
}

// Test that the middleware config does not read values from the environment.
func TestMiddleware_EnvironmentIsolation(t *testing.T) {
	envVals := []string{"true", "false"}
	for _, envVal := range envVals {
		t.Run("AUTHENTICATORS_NOOP_ENABLED="+envVal, func(t *testing.T) {
			t.Setenv("AUTHENTICATORS_NOOP_ENABLED", envVal)

			configFile := writeTestConfigf(t, "")
			mw, err := middleware.New(t.Context(),
				middleware.WithConfigFile(configFile),
			)
			require.NoError(t, err)

			assert.Equal(t, false, mw.Registry().Config().Get("authenticators.noop.enabled"))
		})
	}
}

func TestMiddleware_LoadRulesFromJSON(t *testing.T) {
	jsonRule := `
{
  "authenticators": [
    { "handler": "noop" }
  ],
  "authorizer": {
   "handler": "allow"
  },
  "id": "some-rule-id",
  "match": {
   "methods": [
    "POST"
   ],
   "url": "<(https|http)>://example.com:8080/service/webhooks<(|/.*)>"
  },
  "mutators": [
    { "handler": "noop" }
  ],
  "upstream": {
   "preserve_host": true,
   "strip_path": "/service",
   "url": "http://example.svc.cluster.local"
  }
}`
	var expected rule.Rule
	require.NoError(t, json.Unmarshal([]byte(jsonRule), &expected))
	rulesFile := writeTestConfigf(t, "[%s]", jsonRule)

	configFile := writeTestConfigf(t, `
access_rules:
  matching_strategy: regexp
  repositories:
  - file://%s
authenticators:
  noop:
    enabled: true
authorizers:
  allow:
    enabled: true
mutators:
  noop:
    enabled: true
`, rulesFile)

	mw, err := middleware.New(t.Context(), middleware.WithConfigFile(configFile))
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	actual, err := mw.Registry().RuleRepository().Get(t.Context(), "some-rule-id")
	require.NoError(t, err)
	assert.Equal(t, &expected, actual)
}
