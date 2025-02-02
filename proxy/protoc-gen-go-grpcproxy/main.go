/* Copyright (c) 2019 Snowflake Inc. All rights reserved.

   Licensed under the Apache License, Version 2.0 (the
   "License"); you may not use this file except in compliance
   with the License.  You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing,
   software distributed under the License is distributed on an
   "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
   KIND, either express or implied.  See the License for the
   specific language governing permissions and limitations
   under the License.
*/

package main

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	protogen.Options{}.Run(func(plugin *protogen.Plugin) error {
		plugin.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
		for _, file := range plugin.Files {
			if !file.Generate {
				continue
			}
			generate(plugin, file)
		}
		return nil
	})
}

const (
	contextPackage   = protogen.GoImportPath("context")
	grpcPackage      = protogen.GoImportPath("google.golang.org/grpc")
	grpcProxyPackage = protogen.GoImportPath("github.com/Snowflake-Labs/sansshell/proxy/proxy")
)

func generate(plugin *protogen.Plugin, file *protogen.File) {
	if len(file.Services) == 0 {
		return
	}

	// In the streaming case we need to import io so check for that.
	needIO := false
	for _, service := range file.Services {
		for _, method := range service.Methods {
			if method.Desc.IsStreamingServer() || method.Desc.IsStreamingClient() {
				needIO = true
			}
		}
	}

	// Create the output file and add the header + includes.
	fn := file.GeneratedFilenamePrefix + "_grpcproxy.pb.go"
	g := plugin.NewGeneratedFile(fn, file.GoImportPath)
	g.P("// Auto generated code by protoc-gen-go-grpcproxy")
	g.P("// DO NOT EDIT")
	g.P()
	g.P("// Adds OneMany versions of RPC methods for use by proxy clients")
	g.P()
	g.P("package ", file.GoPackageName)
	g.P()
	g.P("import (")
	g.P(`"fmt"`)
	if needIO {
		g.P(`"io"`)
	}
	g.P()
	g.P(")")
	g.P()

	for _, service := range file.Services {
		// Since we're adding additional methods on top of those defined we can skip
		// if the service has no methods.
		if len(service.Methods) == 0 {
			continue
		}

		// Need the original names plus our Proxy added ones.
		interfaceName := service.GoName + "Client"
		interfaceNameProxy := interfaceName + "Proxy"
		// Need the internal name of the struct.
		goName := strings.ToLower(interfaceName[:1]) + interfaceName[1:]
		clientStruct := goName
		clientStructProxy := clientStruct + "Proxy"

		// Client interface
		//
		// Have to do this as an embed as we can't just extend the original
		// without replicating the entire grpc plugin and our additions.
		// So we create a new one ending in Proxy which embeds the original
		// and adds our OneMany methods.
		g.P("// ", interfaceNameProxy, " is the superset of ", interfaceName, " which additionally includes the OneMany proxy methods")
		g.P("type ", interfaceNameProxy, " interface {")
		g.P(interfaceName)
		for _, method := range service.Methods {
			methodSignature(false, "", g, service, method)
		}
		g.P("}")
		g.P()
		g.P("// Embed the original client inside of this so we get the other generated methods automatically.")
		g.P("type ", clientStructProxy, " struct {")
		g.P("*", clientStruct)
		g.P("}")
		g.P()

		// Now add a NewFooClientProxy which is equiv to NewFooClient except the
		// object it hands back also had FooOneMany methods. This allows us to use
		// this regardless of using a proxy or not since it also implements Foo methods
		// via embedding and taking any ClientConnInterface (so proxy or the grpc one).
		g.P("// New", interfaceNameProxy, " creates a ", interfaceNameProxy, " for use in proxied connections.")
		g.P("// NOTE: This takes a proxy.Conn instead of a generic ClientConnInterface as the methods here are only valid in proxy.Conn contexts.")
		g.P("func New", interfaceNameProxy, "(cc *", g.QualifiedGoIdent(grpcProxyPackage.Ident("Conn")), ") ", interfaceNameProxy, " {")
		g.P("return &", clientStructProxy, "{New", interfaceName, "(cc).(*", clientStruct, ")}")
		g.P("}")
		g.P()

		// For each method we have to create the typed response struct
		// (which comes over the channel) and then generate the OneMany methods.
		var numStreams int
		for _, method := range service.Methods {
			unary := !method.Desc.IsStreamingClient() && !method.Desc.IsStreamingServer()
			clientOnly := method.Desc.IsStreamingClient() && !method.Desc.IsStreamingServer()
			g.P("// ", method.GoName, "ManyResponse encapsulates a proxy data packet.")
			g.P("// It includes the target, index, response and possible error returned.")
			g.P("type ", method.GoName, "ManyResponse struct {")
			g.P("Target string")
			g.P("// As targets can be duplicated this is the index into the slice passed to proxy.Conn.")
			g.P("Index int")
			g.P("Resp *", g.QualifiedGoIdent(method.Output.GoIdent))
			g.P("Error error")
			g.P("}")
			g.P()

			methodStruct := method.GoName + "ClientProxy"
			if !unary {
				// If this isn't a unary RPC we need a number of support structs/interface/methods
				// depending on the type of streaming (client, server or both).
				g.P("type ", service.GoName, "_", methodStruct, " interface {")
				if method.Desc.IsStreamingClient() {
					g.P("Send(*", g.QualifiedGoIdent(method.Input.GoIdent), ") error")
				}
				if clientOnly {
					g.P("CloseAndRecv() ([]*", method.GoName, "ManyResponse, error)")
				}
				if method.Desc.IsStreamingServer() {
					g.P("Recv() ([]*", method.GoName, "ManyResponse, error)")
				}
				g.P(g.QualifiedGoIdent(grpcPackage.Ident("ClientStream")))
				g.P("}")
				g.P()
				g.P("type ", clientStruct, methodStruct, " struct {")
				g.P("cc *", g.QualifiedGoIdent(grpcProxyPackage.Ident("Conn")))
				g.P("directDone bool")
				g.P(g.QualifiedGoIdent(grpcPackage.Ident("ClientStream")))
				g.P("}")
				g.P()

				funcPrelude := fmt.Sprintf("func (x *%s%s) ", clientStruct, methodStruct)
				if method.Desc.IsStreamingClient() {
					// Client streaming (or bidi) need Send
					g.P(funcPrelude, "Send(m *", g.QualifiedGoIdent(method.Input.GoIdent), ") error {")
					g.P("return x.ClientStream.SendMsg(m)")
					g.P("}")
					g.P()
				}
				if clientOnly {
					// If it's client and not bidi need CloseAndRecv() which cleans up the stream after
					// sending the initial request.
					g.P(funcPrelude, "CloseAndRecv() ([]*", method.GoName, "ManyResponse, error) {")
					g.P("if err := x.ClientStream.CloseSend(); err != nil {")
					g.P("return nil, err")
					g.P("}")
					// The rest is the same as Recv() below which we'll fill in there.
				}
				if method.Desc.IsStreamingServer() || clientOnly {
					// Server streaming (or bidi) needs Recv. This is a bit more complicated than
					// grpc base impl because we have to convert a slice of *proxy.Ret back into
					// the proper typed slice the caller expects. We also need to handle the case where
					// we're invoked with no proxy as that code uses a standard grpc.ClientStream which
					// expects different behaviors for the RecvMsg call.

					// We only emit the function signature for the server case. For clientOnly we need
					// the body since Recv handling is the same there.
					if method.Desc.IsStreamingServer() {
						g.P(funcPrelude, "Recv() ([]*", method.GoName, "ManyResponse, error) {")
					}
					g.P("var ret []*", method.GoName, "ManyResponse")
					g.P("// If this is a direct connection the RecvMsg call is to a standard grpc.ClientStream")
					g.P("// and not our proxy based one. This means we need to receive a typed response and")
					g.P("// convert it into a single slice entry return. This ensures the OneMany style calls")
					g.P("// can be used by proxy with 1:N targets and non proxy with 1 target without client changes.")
					g.P("if x.cc.Direct() {")
					g.P("// Check if we're done. Just return EOF now. Any real error was already sent inside")
					g.P("// of a ManyResponse.")
					g.P("if x.directDone {")
					g.P("return nil, io.EOF")
					g.P("}")
					g.P("m := &", g.QualifiedGoIdent(method.Output.GoIdent), "{}")
					g.P("err := x.ClientStream.RecvMsg(m)")
					g.P("ret = append(ret, &", method.GoName, "ManyResponse{")
					g.P("Resp:   m,")
					g.P("Error: err,")
					g.P("Target: x.cc.Targets[0],")
					g.P("Index:  0,")
					g.P("})")
					g.P("// An error means we're done so set things so a later call now gets an EOF.")
					g.P("if err != nil {")
					g.P("x.directDone = true")
					g.P("}")
					g.P("return ret, nil")
					g.P("}")
					g.P()
					// Need a section where we add a loop around RecvMsg since we have to N of these on a CloseSent
					// to ensure we pull all states off.
					if clientOnly {
						g.P("eof := make(map[int]bool)")
						g.P("for i := range x.cc.Targets {")
						g.P("eof[i] = false")
						g.P("}")
						g.P("for {")
						g.P("// Need to allow all client channels to return state before we return since")
						g.P("// no more Recv's will ever be called.")
						g.P("done := true")
						g.P("for _, v := range eof {")
						g.P("if !v {")
						g.P("done = false")
						g.P("}")
						g.P("}")
						g.P("if done {")
						g.P("break")
						g.P("}")
					}
					g.P("m := []*", g.QualifiedGoIdent(grpcProxyPackage.Ident("Ret")), "{}")
					g.P("if err := x.ClientStream.RecvMsg(&m); err != nil {")
					g.P("return nil, err")
					g.P("}")
					g.P("for _, r := range m {")
					g.P("typedResp := &", method.GoName, "ManyResponse{")
					g.P("Resp: &", g.QualifiedGoIdent(method.Output.GoIdent), "{},")
					g.P("}")
					g.P("typedResp.Target = r.Target")
					g.P("typedResp.Index = r.Index")
					g.P("typedResp.Error = r.Error")
					g.P("if r.Error == nil {")
					g.P("if err := r.Resp.UnmarshalTo(typedResp.Resp); err != nil {")
					g.P(`typedResp.Error = fmt.Errorf("can't decode any response - %v. Original Error - %v", err, r.Error)`)
					g.P("}")
					g.P("}")
					g.P("ret = append(ret, typedResp)")
					g.P("}")
					if clientOnly {
						g.P("}")
					}
					g.P("return ret, nil")
					g.P("}")
					g.P()
				}
			}
			methodSignature(true, clientStructProxy, g, service, method)
			if unary {
				// Unary is simple since we send one thing and just loop over a channel waiting
				// for replies. The only annoyance is type converting from Any in the InvokeMany
				// to the typed response callers expect.
				g.P("conn := c.cc.(*", g.QualifiedGoIdent(grpcProxyPackage.Ident("Conn")), ")")
				g.P("ret := make(chan *", method.GoName, "ManyResponse)")
				g.P("// If this is a single case we can just use Invoke and marshal it onto the channel once and be done.")
				g.P("if len(conn.Targets) == 1 {")
				g.P("go func() {")
				g.P("out := &", method.GoName, "ManyResponse{")
				g.P("Target: conn.Targets[0],")
				g.P("Index: 0,")
				g.P("Resp: &", g.QualifiedGoIdent(method.Output.GoIdent), "{},")
				g.P("}")
				g.P("err := conn.Invoke(ctx, \"/", service.Desc.FullName(), "/", method.Desc.Name(), "\", in, out.Resp, opts...)")
				g.P("if err != nil {")
				g.P("out.Error = err")
				g.P("}")
				g.P("// Send and close.")
				g.P("ret <- out")
				g.P("close(ret)")
				g.P("}()")
				g.P("return ret, nil")
				g.P("}")
				g.P("manyRet, err := conn.InvokeOneMany(ctx, \"/", service.Desc.FullName(), "/", method.Desc.Name(), "\", in, opts...)")
				g.P("if err != nil {")
				g.P("return nil, err")
				g.P("}")
				g.P("// A goroutine to retrive untyped responses and convert them to typed ones.")
				g.P("go func() {")
				g.P("for {")
				g.P("typedResp := &", method.GoName, "ManyResponse{")
				g.P("Resp: &", g.QualifiedGoIdent(method.Output.GoIdent), "{},")
				g.P("}")
				g.P()
				g.P("resp, ok := <-manyRet")
				g.P("if !ok {")
				g.P("// All done so we can shut down.")
				g.P("close(ret)")
				g.P("return")
				g.P("}")
				g.P("typedResp.Target = resp.Target")
				g.P("typedResp.Index = resp.Index")
				g.P("typedResp.Error = resp.Error")
				g.P("if resp.Error == nil {")
				g.P("if err := resp.Resp.UnmarshalTo(typedResp.Resp); err != nil {")
				g.P(`typedResp.Error = fmt.Errorf("can't decode any response - %v. Original Error - %v", err, resp.Error)`)
				g.P("}")
				g.P("}")
				g.P("ret <- typedResp")
				g.P("}")
				g.P("}()")
				g.P()
				g.P("return ret, nil")
				g.P("}")
				g.P()
				continue
			}

			// If it's not unary we create a stream and return it.
			// Client only streams also get the request in so they send it and close the sending end before returning.
			g.P("stream, err := c.cc.NewStream(ctx, &", service.GoName, "_", "ServiceDesc.Streams[", numStreams, "], \"/", service.Desc.FullName(), "/", method.Desc.Name(), "\", opts...)")
			g.P("if err != nil {")
			g.P("return nil, err")
			g.P("}")
			g.P("x := &", clientStruct, methodStruct, "{c.cc.(*", g.QualifiedGoIdent(grpcProxyPackage.Ident("Conn")), "), false, stream}")
			if !method.Desc.IsStreamingClient() {
				g.P("if err := x.ClientStream.SendMsg(in); err != nil {")
				g.P("return nil, err")
				g.P("}")
				g.P("if err := x.ClientStream.CloseSend(); err != nil {")
				g.P("return nil, err")
				g.P("}")
			}
			g.P("return x, nil")
			g.P("}")
			numStreams++
			g.P()
		}
	}
}

// methodSignature generates the function signature for a OneMany method in both interface form
// and when actually declaring the function (by adding the trailing open brace). It's a little
// clunky but g.P() always adds a newline.
func methodSignature(genFunc bool, structName string, g *protogen.GeneratedFile, service *protogen.Service, method *protogen.Method) {
	prefix := ""
	if genFunc {
		prefix = fmt.Sprintf("// %sOneMany provides the same API as %s but sends the same request to N destinations at once.\n", method.GoName, method.GoName)
		prefix += "// N can be a single destination.\n"
		prefix += "//\n// NOTE: The returned channel must be read until it closes in order to avoid leaking goroutines.\n"
		prefix += "func (c *" + structName + ") "
	}
	sig := fmt.Sprintf("%s%sOneMany(ctx %s, ", prefix, method.GoName, g.QualifiedGoIdent(contextPackage.Ident("Context")))
	unary := !method.Desc.IsStreamingClient() && !method.Desc.IsStreamingServer()
	serverOnly := !method.Desc.IsStreamingClient() && method.Desc.IsStreamingServer()
	if unary || serverOnly {
		sig = fmt.Sprintf("%s in *%s, ", sig, g.QualifiedGoIdent(method.Input.GoIdent))
	}
	brace := ""
	if genFunc {
		brace = " {"
	}
	returnTypes := fmt.Sprintf("(<-chan *%sManyResponse, error)", method.GoName)
	if !unary {
		returnTypes = fmt.Sprintf("(%s_%sClientProxy, error)", service.GoName, method.GoName)
	}
	g.P(sig, "opts ...", g.QualifiedGoIdent(grpcPackage.Ident("CallOption")), ") ", returnTypes, brace)
}
