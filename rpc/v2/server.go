// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package v2

import (
	"fmt"
	"reflect"
	"runtime"
	"time"

	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"golang.org/x/net/context"
)

// NewServer will create a new server instance with no registered handlers.
func NewServer() *Server {
	server := &Server{services: make(serviceRegistry), subscriptions: make(subscriptionRegistry)}

	// register a default service which will provide meta information about the RPC service such as the services and
	// methods it offers.
	rpcService := &RPCService{server}
	server.RegisterName("rpc", rpcService)

	return server
}

// NewServerWithTimeout will create a new server instance with no registered
// handlers. It also sets a context timeout for each request. Methods that receive
// a context will be cancelled after the specified duration.
func NewServerWithTimeout(t time.Duration) *Server {
	s := NewServer()
	s.timeOut = t
	return s
}

// RPCService gives meta information about the server.
// e.g. gives information about the loaded modules.
type RPCService struct {
	server *Server
}

// Modules returns the list of RPC services with their version number
func (s *RPCService) Modules() map[string]string {
	modules := make(map[string]string)
	for name, _ := range s.server.services {
		modules[name] = "1.0"
	}
	return modules
}

// RegisterName will create an service for the given rcvr type under the given name. When no methods on the given rcvr
// match the criteria to be either a RPC method or a subscription an error is returned. Otherwise a new service is
// created and added to the service collection this server instance serves.
func (s *Server) RegisterName(name string, rcvr interface{}) error {
	if s.services == nil {
		s.services = make(serviceRegistry)
	}

	svc := new(service)
	svc.typ = reflect.TypeOf(rcvr)
	rcvrVal := reflect.ValueOf(rcvr)

	if name == "" {
		return fmt.Errorf("no service name for type %s", svc.typ.String())
	}
	if !isExported(reflect.Indirect(rcvrVal).Type().Name()) {
		return fmt.Errorf("%s is not exported", reflect.Indirect(rcvrVal).Type().Name())
	}

	// already a previous service register under given sname, merge methods/subscriptions
	if regsvc, present := s.services[name]; present {
		methods, subscriptions := suitableCallbacks(rcvrVal, svc.typ)
		if len(methods) == 0 && len(subscriptions) == 0 {
			return fmt.Errorf("Service doesn't have any suitable methods/subscriptions to expose")
		}

		for _, m := range methods {
			regsvc.callbacks[formatName(m.method.Name)] = m
		}
		for _, s := range subscriptions {
			regsvc.subscriptions[formatName(s.method.Name)] = s
		}

		return nil
	}

	svc.name = name
	svc.callbacks, svc.subscriptions = suitableCallbacks(rcvrVal, svc.typ)

	if len(svc.callbacks) == 0 && len(svc.subscriptions) == 0 {
		return fmt.Errorf("Service doesn't have any suitable methods/subscriptions to expose")
	}

	s.services[svc.name] = svc

	return nil
}

// ServeCodec reads incoming requests from codec, calls the appropriate callback and writes the
// response back using the given codec. It will block until the codec is closed.
//
// This server will:
// 1. allow for asynchronous and parallel request execution
// 2. supports notifications (pub/sub)
// 3. supports request batches
func (s *Server) ServeCodec(codec ServerCodec) {
	pctx, pcancel := context.WithCancel(context.Background())

	defer func() {
		pcancel()
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			glog.Errorln(string(buf))
		}
		codec.Close()
	}()

	for {
		reqs, batch, err := s.readRequest(codec)
		if err != nil {
			glog.V(logger.Debug).Infof("%v\n", err)
			codec.Write(codec.CreateErrorResponse(nil, err))
			break
		}

		var ctx context.Context
		var cancel context.CancelFunc
		if s.timeOut == 0 {
			ctx, cancel = context.WithCancel(pctx)
		} else {
			ctx, cancel = context.WithTimeout(pctx, s.timeOut)
		}

		if batch {
			go func() {
				s.execBatch(ctx, codec, reqs)
				cancel()
			}()
		} else {
			go func() {
				s.exec(ctx, codec, reqs[0])
				cancel()
			}()
		}
	}
}

// sendNotification will create a notification from the given event by serializing member fields of the event.
// It will then send the notification to the client, when it fails the codec is closed. When the event has multiple
// fields an array of values is returned.
func sendNotification(codec ServerCodec, subid string, event interface{}) {
	notification := codec.CreateNotification(subid, event)

	if err := codec.Write(notification); err != nil {
		codec.Close()
	}
}

// createSubscription will register a new subscription and waits for raised events. When an event is raised it will:
// 1. test if the event is raised matches the criteria the user has (optionally) specified
// 2. create a notification of the event and send it the client when it matches the criteria
// It will unsubscribe the subscription when the socket is closed or the subscription is unsubscribed by the user.
func (s *Server) createSubscription(c ServerCodec, req *serverRequest) (string, error) {
	args := []reflect.Value{req.callb.rcvr}
	if len(req.args) > 0 {
		args = append(args, req.args...)
	}

	subid, err := newSubscriptionId()
	if err != nil {
		return "", err
	}

	reply := req.callb.method.Func.Call(args)

	if reply[1].IsNil() { // no error
		if subscription, ok := reply[0].Interface().(Subscription); ok {
			s.muSubcriptions.Lock()
			s.subscriptions[subid] = subscription
			s.muSubcriptions.Unlock()
			go func() {
				cases := []reflect.SelectCase{
					reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(subscription.Chan())}, // new event
					reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(c.Closed())},          // connection closed
				}

				for {
					idx, notification, recvOk := reflect.Select(cases)
					switch idx {
					case 0: // new event, or channel closed
						if recvOk { // send notification
							if event, ok := notification.Interface().(*event.Event); ok {
								if subscription.match == nil || subscription.match(event.Data) {
									sendNotification(c, subid, subscription.format(event.Data))
								}
							}
						} else { // user send an eth_unsubscribe request
							return
						}
					case 1: // connection closed
						s.unsubscribe(subid)
						return
					}
				}
			}()
		} else { // unable to create subscription
			s.muSubcriptions.Lock()
			delete(s.subscriptions, subid)
			s.muSubcriptions.Unlock()
		}
	} else {
		return "", fmt.Errorf("Unable to create subscription")
	}

	return subid, nil
}

// unsubscribe calls the Unsubscribe method on the subscription and removes a subscription from the subscription
// registry.
func (s *Server) unsubscribe(subid string) bool {
	s.muSubcriptions.Lock()
	defer s.muSubcriptions.Unlock()
	if sub, ok := s.subscriptions[subid]; ok {
		sub.Unsubscribe()
		delete(s.subscriptions, subid)
		return true
	}
	return false
}

// exec executes the given request and writes the result back using the codec.
func (s *Server) exec(ctx context.Context, codec ServerCodec, req *serverRequest) {
	if req.err != nil { // error during request parsing
		rpcErr := codec.CreateErrorResponse(&req.id, req.err)
		if err := codec.Write(rpcErr); err != nil {
			codec.Close()
		}
		return
	}

	if req.isUnsubscribe { // first param must be the subscription id
		if len(req.args) >= 1 && req.args[0].Kind() == reflect.String {
			subid := req.args[0].String()
			if s.unsubscribe(subid) {
				if err := codec.Write(codec.CreateResponse(req.id, true)); err != nil {
					codec.Close()
				}
			} else {
				rpcErr := codec.CreateErrorResponse(&req.id,
					&callbackError{fmt.Sprintf("subscription '%s' not found", subid)})
				if err := codec.Write(rpcErr); err != nil {
					codec.Close()
				}
			}
		} else {
			rpcErr := codec.CreateErrorResponse(&req.id, &invalidParamsError{"Expected subscription id as argument"})
			if err := codec.Write(rpcErr); err != nil {
				codec.Close()
			}
		}
		return
	}

	if req.callb.isSubscribe {
		subid, err := s.createSubscription(codec, req)
		var response interface{}
		if err == nil {
			response = codec.CreateResponse(req.id, subid)
		} else {
			response = codec.CreateErrorResponse(&req.id, &callbackError{err.Error()})
		}

		if err = codec.Write(response); err != nil {
			codec.Close()
		}
		return
	}

	// regular RPC call
	if len(req.args) != len(req.callb.argTypes) {
		rpcErr := &invalidParamsError{fmt.Sprintf("%s%s%s expects %d parameters, got %d",
			req.svcname, serviceMethodSeparator, req.callb.method.Name,
			len(req.callb.argTypes), len(req.args))}

		res := codec.CreateErrorResponse(&req.id, rpcErr)
		if err := codec.Write(res); err != nil {
			codec.Close()
		}
		return
	}

	arguments := []reflect.Value{req.callb.rcvr}
	if req.callb.hasCtx {
		arguments = append(arguments, reflect.ValueOf(ctx))
	}
	if len(req.args) > 0 {
		arguments = append(arguments, req.args...)
	}

	reply := req.callb.method.Func.Call(arguments)

	if len(reply) == 0 {
		if err := codec.Write(codec.CreateResponse(req.id, nil)); err != nil {
			codec.Close()
		}
		return
	}

	if req.callb.errPos >= 0 { // test if method returned an error
		if !reply[req.callb.errPos].IsNil() {
			e := reply[req.callb.errPos].Interface().(error)
			res := codec.CreateErrorResponse(&req.id, &callbackError{e.Error()})
			if err := codec.Write(res); err != nil {
				codec.Close()
			}
			return
		}
	}

	if err := codec.Write(codec.CreateResponse(req.id, reply[0].Interface())); err != nil {
		codec.Close()
	}
}

// execBatch executes the given requests and writes the result back using the codec. It will only write the response
// back when the last request is processed.
func (s *Server) execBatch(ctx context.Context, codec ServerCodec, requests []*serverRequest) {
	responses := make([]interface{}, len(requests))

	for i, req := range requests {
		if req.err != nil { // error during parsing of request
			responses[i] = codec.CreateErrorResponse(&req.id, req.err)
			continue
		}

		if req.isUnsubscribe {
			if len(req.args) == 1 && req.args[0].Kind() == reflect.String {
				subid := req.args[0].String()
				if s.unsubscribe(subid) {
					responses[i] = codec.CreateResponse(req.id, true)
				} else {
					e := &callbackError{fmt.Sprintf("subscription '%s' not found", subid)}
					responses[i] = codec.CreateErrorResponse(&req.id, e)
				}
			} else {
				e := &invalidParamsError{"Expected subscription id as argument"}
				responses[i] = codec.CreateErrorResponse(&req.id, e)
			}
			continue
		}

		if req.callb.isSubscribe {
			subid, err := s.createSubscription(codec, req)
			var response interface{}
			if err == nil {
				response = codec.CreateResponse(req.id, subid)
			} else {
				response = codec.CreateErrorResponse(&req.id, &callbackError{err.Error()})
			}

			responses[i] = response
			continue
		}

		var reply []reflect.Value

		if len(req.args) != len(req.callb.argTypes) {
			rpcErr := &invalidParamsError{fmt.Sprintf("%s%s%s expects %d parameters, got %d",
				req.svcname, serviceMethodSeparator, req.callb.method.Name, len(req.callb.argTypes), len(req.args))}
			responses[i] = codec.CreateErrorResponse(&req.id, rpcErr)
			continue
		}

		arguments := []reflect.Value{req.callb.rcvr}
		if req.callb.hasCtx {
			arguments = append(arguments, reflect.ValueOf(ctx))
		}
		if len(req.args) > 0 {
			arguments = append(arguments, req.args...)
		}

		reply = req.callb.method.Func.Call(arguments)

		if len(reply) == 0 {
			responses[i] = codec.CreateResponse(req.id, nil)
			continue
		}

		if req.callb.errPos >= 0 {
			if !reply[req.callb.errPos].IsNil() {
				if e, ok := reply[req.callb.errPos].Interface().(error); ok {
					rpcErr := &callbackError{e.Error()}
					responses[i] = codec.CreateErrorResponse(&req.id, rpcErr)
					continue
				}
			}
		}

		responses[i] = codec.CreateResponse(req.id, reply[0].Interface())
	}

	if err := codec.Write(responses); err != nil {
		glog.V(logger.Error).Infof("%v\n", err)
		codec.Close()
	}
}

// readRequest requests the next (batch) request from the codec. It will return the collection of requests, an
// indication if the request was a batch, the invalid request identifier and an error when the request could not be
// read/parsed.
func (s *Server) readRequest(codec ServerCodec) ([]*serverRequest, bool, RPCError) {
	reqs, batch, err := codec.ReadRequestHeaders()
	if err != nil {
		return nil, batch, err
	}

	requests := make([]*serverRequest, len(reqs))

	// verify requests
	for i, r := range reqs {
		var ok bool
		var svc *service

		if r.isPubSub && r.method == unsubscribeMethod {
			requests[i] = &serverRequest{id: r.id, isUnsubscribe: true}
			argTypes := []reflect.Type{reflect.TypeOf("")}
			if args, err := codec.ParseRequestArguments(argTypes, r.params); err == nil {
				requests[i].args = args
			} else {
				requests[i].err = &invalidParamsError{err.Error()}
			}
			continue
		}

		if svc, ok = s.services[r.service]; !ok {
			requests[i] = &serverRequest{id: r.id, err: &methodNotFoundError{r.service, r.method}}
			continue
		}

		if r.isPubSub { // eth_subscribe
			if callb, ok := svc.subscriptions[r.method]; ok {
				requests[i] = &serverRequest{id: r.id, svcname: svc.name, callb: callb}
				if r.params != nil && len(callb.argTypes) > 0 {
					argTypes := []reflect.Type{reflect.TypeOf("")}
					argTypes = append(argTypes, callb.argTypes...)
					if args, err := codec.ParseRequestArguments(argTypes, r.params); err == nil {
						requests[i].args = args[1:] // first one is service.method name which isn't an actual argument
					} else {
						requests[i].err = &invalidParamsError{err.Error()}
					}
				}
			} else {
				requests[i] = &serverRequest{id: r.id, err: &methodNotFoundError{subscribeMethod, r.method}}
			}
			continue
		}

		if callb, ok := svc.callbacks[r.method]; ok {
			requests[i] = &serverRequest{id: r.id, svcname: svc.name, callb: callb}
			if r.params != nil && len(callb.argTypes) > 0 {
				if args, err := codec.ParseRequestArguments(callb.argTypes, r.params); err == nil {
					requests[i].args = args
				} else {
					requests[i].err = &invalidParamsError{err.Error()}
				}
			}
			continue
		}

		requests[i] = &serverRequest{id: r.id, err: &methodNotFoundError{r.service, r.method}}
	}

	return requests, batch, nil
}