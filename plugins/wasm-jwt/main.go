package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"

	"github.com/proxy-wasm/proxy-wasm-go-sdk/proxywasm"
	"github.com/proxy-wasm/proxy-wasm-go-sdk/proxywasm/types"
)

const (
	headerAuth   = "authorization"
	headerUID    = "x-uid"
	bearerPrefix = "Bearer "
)

type jwtHeader struct {
	Alg string `json:"alg"`
}

type jwtClaims struct {
	Sub string `json:"sub"`
}

type rawConfig struct {
	PublicKeyPEM string `json:"public_key_pem"`
}

type vmContext struct {
	types.DefaultVMContext
}

type pluginContext struct {
	types.DefaultPluginContext
	state *pluginState
}

type httpContext struct {
	types.DefaultHttpContext
	state *pluginState
}

type pluginState struct {
	publicKey *rsa.PublicKey
	configErr error
}

func main() {
}

func init() {
	proxywasm.SetVMContext(&vmContext{})
}

func (*vmContext) NewPluginContext(contextID uint32) types.PluginContext {
	return &pluginContext{state: &pluginState{}}
}

func (ctx *pluginContext) OnPluginStart(pluginConfigurationSize int) types.OnPluginStartStatus {
	state, err := loadConfig()
	if err != nil {
		ctx.state.configErr = err
		proxywasm.LogWarnf("plugin config error: %v", err)
		return types.OnPluginStartStatusOK
	}
	ctx.state = state
	proxywasm.LogInfo("proxy-wasm plugin started")
	return types.OnPluginStartStatusOK
}

func (ctx *pluginContext) NewHttpContext(contextID uint32) types.HttpContext {
	return &httpContext{state: ctx.state}
}

func (ctx *httpContext) OnHttpRequestHeaders(numHeaders int, endOfStream bool) types.Action {
	if ctx.state == nil || ctx.state.configErr != nil || ctx.state.publicKey == nil {
		return ctx.deny("denied: plugin config is invalid")
	}

	authHeader, err := proxywasm.GetHttpRequestHeader(headerAuth)
	if err != nil {
		if err == types.ErrorStatusNotFound {
			return ctx.deny("denied: authorization header is missing")
		}
		proxywasm.LogWarnf("authorization header lookup failed: %v", err)
		return ctx.deny("denied: authorization header lookup failed")
	}
	if authHeader == "" {
		return ctx.deny("denied: authorization header is missing")
	}
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		return ctx.deny("denied: authorization header is invalid")
	}
	token := strings.TrimPrefix(authHeader, bearerPrefix)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ctx.deny("denied: token format is invalid")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ctx.deny("denied: token header is invalid")
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return ctx.deny("denied: token header is invalid")
	}
	if header.Alg != "RS256" {
		return ctx.deny("denied: token header is invalid")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ctx.deny("denied: token payload is invalid")
	}
	var claims jwtClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return ctx.deny("denied: token payload is invalid")
	}
	if claims.Sub == "" {
		return ctx.deny("denied: subject is missing")
	}

	signingInput := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ctx.deny("denied: token signature is invalid")
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(ctx.state.publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return ctx.deny("denied: token signature is invalid")
	}

	if err := proxywasm.ReplaceHttpRequestHeader(headerUID, claims.Sub); err != nil {
		if err == types.ErrorStatusNotFound {
			if err := proxywasm.AddHttpRequestHeader(headerUID, claims.Sub); err != nil {
				proxywasm.LogWarnf("add %s header failed: %v", headerUID, err)
			}
		} else {
			proxywasm.LogWarnf("set %s header failed: %v", headerUID, err)
		}
	}
	return types.ActionContinue
}

func (ctx *httpContext) deny(reason string) types.Action {
	_ = proxywasm.SendHttpResponse(403,
		[][2]string{{"content-type", "text/plain"}},
		[]byte(reason),
		-1,
	)
	return types.ActionPause
}

func loadConfig() (*pluginState, error) {
	raw, err := proxywasm.GetPluginConfiguration()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var cfg rawConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	cfg.PublicKeyPEM = strings.ReplaceAll(cfg.PublicKeyPEM, "\\n", "\n")
	block, _ := pem.Decode([]byte(cfg.PublicKeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("public key PEM is invalid")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	publicKey, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("public key type is invalid")
	}
	return &pluginState{publicKey: publicKey}, nil
}
