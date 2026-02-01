package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	auth "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoytype "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/lestrrat-go/jwx/v3/jwa"
	jwxjwt "github.com/lestrrat-go/jwx/v3/jwt"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	bearerPrefix = "Bearer "
	headerAuth   = "authorization"
	headerUID    = "x-uid"
)

var (
	errMissingAuthorization = errors.New("authorization header is missing")
	errInvalidAuthorization = errors.New("authorization header is invalid")
)

type calloutServer struct {
	publicKey *rsa.PublicKey
}

func newCalloutServer(publicKey *rsa.PublicKey) (*calloutServer, error) {
	if publicKey == nil {
		return nil, errors.New("public key is nil")
	}
	return &calloutServer{publicKey: publicKey}, nil
}

func (s *calloutServer) Check(ctx context.Context, req *auth.CheckRequest) (*auth.CheckResponse, error) {
	bearer, err := bearerFromRequest(req)
	if err != nil {
		return buildDeniedResponse(int32(codes.PermissionDenied), err.Error()), nil
	}

	token, err := jwxjwt.Parse([]byte(bearer), jwxjwt.WithKey(jwa.RS256(), s.publicKey), jwxjwt.WithValidate(true))
	if err != nil {
		return buildDeniedResponse(int32(codes.PermissionDenied), err.Error()), nil
	}
	sub, ok := token.Subject()
	if !ok || sub == "" {
		return buildDeniedResponse(int32(codes.PermissionDenied), "subject is missing"), nil
	}

	return buildOkResponse(sub), nil
}

func bearerFromRequest(req *auth.CheckRequest) (string, error) {
	httpAttrs := req.GetAttributes().GetRequest().GetHttp()
	// Service Extensions ext_authz can populate header_map instead of headers,
	// so we read from header_map here. If your environment fills headers,
	// adjust this to read the headers field instead.
	return bearerFromHeaderMap(httpAttrs.GetHeaderMap())
}

func bearerFromHeaderMap(headerMap *core.HeaderMap) (string, error) {
	if value := getHeaderValueFromHeaderMap(headerMap, headerAuth); value != "" {
		return parseBearer(value)
	}
	return "", errMissingAuthorization
}

func parseBearer(value string) (string, error) {
	if !strings.HasPrefix(value, bearerPrefix) {
		return "", errInvalidAuthorization
	}
	return strings.TrimPrefix(value, bearerPrefix), nil
}

func getHeaderValueFromHeaderMap(headerMap *core.HeaderMap, key string) string {
	if headerMap == nil {
		return ""
	}
	for _, header := range headerMap.GetHeaders() {
		if !strings.EqualFold(header.GetKey(), key) {
			continue
		}
		value := header.GetValue()
		if value == "" && len(header.GetRawValue()) > 0 {
			value = string(header.GetRawValue())
		}
		if value != "" {
			return value
		}
	}
	return ""
}

func buildOkResponse(uid string) *auth.CheckResponse {
	return &auth.CheckResponse{
		Status: &status.Status{Code: int32(codes.OK)},
		HttpResponse: &auth.CheckResponse_OkResponse{
			OkResponse: &auth.OkHttpResponse{
				Headers: []*core.HeaderValueOption{
					{
						Header: &core.HeaderValue{Key: headerUID, Value: uid, RawValue: []byte(uid)},
						Append: wrapperspb.Bool(false),
					},
				},
			},
		},
	}
}

func buildDeniedResponse(code int32, msg string) *auth.CheckResponse {
	return &auth.CheckResponse{
		Status: &status.Status{Code: code, Message: msg},
		HttpResponse: &auth.CheckResponse_DeniedResponse{
			DeniedResponse: &auth.DeniedHttpResponse{
				Status: &envoytype.HttpStatus{Code: envoytype.StatusCode_Forbidden},
				Body:   fmt.Sprintf("denied: %s", msg),
			},
		},
	}
}

func parseRSAPublicKey(pemString string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemString))
	if block == nil {
		return nil, errors.New("invalid PEM data")
	}

	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("unsupported PEM type: %s", block.Type)
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("invalid key type")
	}
	return rsaPub, nil
}

func (s *calloutServer) Process(stream extproc.ExternalProcessor_ProcessServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		resp, err := s.handleProcessingRequest(req)
		if err != nil {
			return err
		}
		if resp == nil {
			continue
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (s *calloutServer) handleProcessingRequest(req *extproc.ProcessingRequest) (*extproc.ProcessingResponse, error) {
	if headers := req.GetRequestHeaders(); headers != nil {
		return s.handleRequestHeaders(headers)
	}
	return buildContinueProcessingResponse(req), nil
}

func (s *calloutServer) handleRequestHeaders(headers *extproc.HttpHeaders) (*extproc.ProcessingResponse, error) {
	bearer, err := bearerFromHeaderMap(headers.GetHeaders())
	if err != nil {
		return buildImmediateDeniedProcessingResponse(err.Error()), nil
	}

	token, err := jwxjwt.Parse([]byte(bearer), jwxjwt.WithKey(jwa.RS256(), s.publicKey), jwxjwt.WithValidate(true))
	if err != nil {
		return buildImmediateDeniedProcessingResponse(err.Error()), nil
	}
	sub, ok := token.Subject()
	if !ok || sub == "" {
		return buildImmediateDeniedProcessingResponse("subject is missing"), nil
	}

	return buildRequestHeadersProcessingResponse(sub), nil
}

func buildContinueProcessingResponse(req *extproc.ProcessingRequest) *extproc.ProcessingResponse {
	switch req.GetRequest().(type) {
	case *extproc.ProcessingRequest_RequestHeaders:
		return &extproc.ProcessingResponse{
			Response: &extproc.ProcessingResponse_RequestHeaders{
				RequestHeaders: &extproc.HeadersResponse{
					Response: &extproc.CommonResponse{Status: extproc.CommonResponse_CONTINUE},
				},
			},
		}
	case *extproc.ProcessingRequest_ResponseHeaders:
		return &extproc.ProcessingResponse{
			Response: &extproc.ProcessingResponse_ResponseHeaders{
				ResponseHeaders: &extproc.HeadersResponse{
					Response: &extproc.CommonResponse{Status: extproc.CommonResponse_CONTINUE},
				},
			},
		}
	case *extproc.ProcessingRequest_RequestBody:
		return &extproc.ProcessingResponse{
			Response: &extproc.ProcessingResponse_RequestBody{
				RequestBody: &extproc.BodyResponse{
					Response: &extproc.CommonResponse{Status: extproc.CommonResponse_CONTINUE},
				},
			},
		}
	case *extproc.ProcessingRequest_ResponseBody:
		return &extproc.ProcessingResponse{
			Response: &extproc.ProcessingResponse_ResponseBody{
				ResponseBody: &extproc.BodyResponse{
					Response: &extproc.CommonResponse{Status: extproc.CommonResponse_CONTINUE},
				},
			},
		}
	case *extproc.ProcessingRequest_RequestTrailers:
		return &extproc.ProcessingResponse{
			Response: &extproc.ProcessingResponse_RequestTrailers{
				RequestTrailers: &extproc.TrailersResponse{},
			},
		}
	case *extproc.ProcessingRequest_ResponseTrailers:
		return &extproc.ProcessingResponse{
			Response: &extproc.ProcessingResponse_ResponseTrailers{
				ResponseTrailers: &extproc.TrailersResponse{},
			},
		}
	default:
		return nil
	}
}

func buildRequestHeadersProcessingResponse(uid string) *extproc.ProcessingResponse {
	return &extproc.ProcessingResponse{
		Response: &extproc.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extproc.HeadersResponse{
				Response: &extproc.CommonResponse{
					Status: extproc.CommonResponse_CONTINUE,
					HeaderMutation: &extproc.HeaderMutation{
						SetHeaders: []*core.HeaderValueOption{
							{
								Header: &core.HeaderValue{Key: headerUID, Value: uid, RawValue: []byte(uid)},
								Append: wrapperspb.Bool(false),
							},
						},
					},
				},
			},
		},
	}
}

func buildImmediateDeniedProcessingResponse(msg string) *extproc.ProcessingResponse {
	return &extproc.ProcessingResponse{
		Response: &extproc.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extproc.ImmediateResponse{
				Status: &envoytype.HttpStatus{Code: envoytype.StatusCode_Forbidden},
				Body:   []byte(fmt.Sprintf("denied: %s", msg)),
			},
		},
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	publicKeyPEM := os.Getenv("PUBLIC_KEY_PEM")
	if publicKeyPEM == "" {
		log.Fatalf("config error: PUBLIC_KEY_PEM is required")
	}

	publicKeyPEM = strings.ReplaceAll(publicKeyPEM, "\\n", "\n")
	publicKey, err := parseRSAPublicKey(publicKeyPEM)
	if err != nil {
		log.Fatalf("read public key error: %v", err)
	}

	server, err := newCalloutServer(publicKey)
	if err != nil {
		log.Fatalf("callout server error: %v", err)
	}

	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("listen error: %v", err)
	}

	grpcServer := grpc.NewServer()
	auth.RegisterAuthorizationServer(grpcServer, server)
	extproc.RegisterExternalProcessorServer(grpcServer, server)

	log.Printf("callout-server listening on :%s", port)
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("grpc server error: %v", err)
	}
}
