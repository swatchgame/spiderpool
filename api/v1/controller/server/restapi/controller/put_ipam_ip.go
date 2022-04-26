// Code generated by go-swagger; DO NOT EDIT.

// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

package controller

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the generate command

import (
	"net/http"

	"github.com/go-openapi/runtime/middleware"
)

// PutIpamIPHandlerFunc turns a function with the right signature into a put ipam IP handler
type PutIpamIPHandlerFunc func(PutIpamIPParams) middleware.Responder

// Handle executing the request and returning a response
func (fn PutIpamIPHandlerFunc) Handle(params PutIpamIPParams) middleware.Responder {
	return fn(params)
}

// PutIpamIPHandler interface for that can handle valid put ipam IP params
type PutIpamIPHandler interface {
	Handle(PutIpamIPParams) middleware.Responder
}

// NewPutIpamIP creates a new http.Handler for the put ipam IP operation
func NewPutIpamIP(ctx *middleware.Context, handler PutIpamIPHandler) *PutIpamIP {
	return &PutIpamIP{Context: ctx, Handler: handler}
}

/* PutIpamIP swagger:route PUT /ipam/ip controller putIpamIp

Force set ip

Force set ip for spiderpool controller cli debug usage


*/
type PutIpamIP struct {
	Context *middleware.Context
	Handler PutIpamIPHandler
}

func (o *PutIpamIP) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	route, rCtx, _ := o.Context.RouteInfo(r)
	if rCtx != nil {
		*r = *rCtx
	}
	var Params = NewPutIpamIPParams()
	if err := o.Context.BindValidRequest(r, route, &Params); err != nil { // bind params
		o.Context.Respond(rw, r, route.Produces, route, err)
		return
	}

	res := o.Handler.Handle(Params) // actually handle the request
	o.Context.Respond(rw, r, route.Produces, route, res)

}