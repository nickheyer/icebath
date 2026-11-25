package client

import (
	"net/http"
	"net/url"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectinprocess"
)

type authorized struct {
	user *url.Userinfo
}

func (a authorized) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	password, _ := a.user.Password()
	request.SetBasicAuth(a.user.Username(), password)
	return http.DefaultTransport.RoundTrip(request)
}

func Remote(address string) *connect.Client {
	parsed, err := url.Parse(address)
	if err != nil || parsed.User == nil {
		return connect.NewClient(connecthttp.NewTransport(http.DefaultClient, address))
	}
	user := parsed.User
	parsed.User = nil
	return connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: authorized{user: user}}, parsed.String()))
}

func InProcess(services *connect.Server) *connect.Client {
	return connect.NewClient(connectinprocess.New(services))
}
