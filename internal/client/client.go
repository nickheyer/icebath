package client

import (
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectinprocess"
)

func Remote(url string) *connect.Client {
	return connect.NewClient(connecthttp.NewTransport(http.DefaultClient, url))
}

func InProcess(services *connect.Server) *connect.Client {
	return connect.NewClient(connectinprocess.New(services))
}
