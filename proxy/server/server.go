package server

import (
  "context"
  "io"
  "log"

  "google.golang.org/protobuf/proto"
  "google.golang.org/grpc/codes"
  "google.golang.org/grpc/status"
  "golang.org/x/sync/errgroup"

  pb "github.com/Snowflake-Labs/sansshell/proxy"

)

// server implements proxy.ProxyServer
type server struct {
  // A map of /Package.Service/Method => ServiceMethod
  serviceMap map[string]*ServiceMethod
}

func convertStatus(s *status.Status) (*pb.Status, error) {
  data, err := proto.Marshal(s.Proto())
  if err != nil { return nil, err }
  ps := &pb.Status{}
  if err := proto.Unmarshal(data, ps); err != nil {
    return nil, err
  }
  return ps, nil
}

// Proxy implements Proxy.Proxy to provide a single bidirectional
// stream which manages requests to a set of one or more backend
// target servers.
func (s *server) Proxy(stream pb.Proxy_ProxyServer) error {
  log.Println("Recieved proxy request")

  requestChan := make(chan *pb.ProxyRequest)
  replyChan := make(chan *pb.ProxyReply)

  group, ctx := errgroup.WithContext(stream.Context())

  // create a new TargetStreamSet to manage the target streams
  // associated with this proxy connection.
  streamSet := NewTargetStreamSet(s.serviceMap)

  // A single go-routine for handling all sends to the reply
  // channel.
  // While a stream can be safely used for both send and receive
  // simultaneously, it is not safe for multiple goroutines
  // to call "Send" on the same stream.
  group.Go(func() error {
    return send(replyChan, stream)
  })

  // A single go-routine for receiving all incoming requests from
  // the client.
  // While a stream can be safely used for both send and receive
  // simultaneously, it is not safe for multiple goroutines
  // to call "Recv" on the same stream.
  group.Go(func() error {
    // Close 'requestChan' when receive returns, since we will
    // never receive any additional messages from the client.
    // This can be used by the dispatching goroutine as a single
    // to CloseSend on the target streams.
    defer close(requestChan)

    return receive(ctx, stream, requestChan)
  })

  // This dispatching goroutine manages request dispatch to a set of
  // active target streams.
  group.Go(func() error {
    // when we finish dispatching, we're done, and will send no further
    // messages to the reply channel
    // This will signal the Send goroutine to exit.
    defer close(replyChan)

    // Invoke dispatch to handle incoming requests.
    return dispatch(ctx, requestChan, replyChan, streamSet)
  })

  // Final RPC status is the status of the waitgroup.
  err := group.Wait()
  if err != nil {
    return status.Error(codes.Internal, err.Error())
  }
  return nil
}

// send relays messages from `replyChan` to the provided stream.
func send(replyChan chan *pb.ProxyReply, stream pb.Proxy_ProxyServer) error {
  for msg := range replyChan {
    if err := stream.Send(msg); err != nil {
      return err
    }
  }
  return nil
}

// receive relays incoming messages received from the provided stream to `requestChan`
// until EOF (or other error) is received from the stream, or the supplied context is
// done.
func receive(ctx context.Context, stream pb.Proxy_ProxyServer, requestChan chan *pb.ProxyRequest) error {
  for {
    // Receive from the client stream.
    // This will block, but can return early
    // if the stream context is cancelled.
    req, err := stream.Recv()
    if err == io.EOF {
      // On the server, io.EOF indicates that the
      // client has issued as CloseSend(), and will
      // issue no further requests.
      // Returning here will close requestChan, which
      // we can use as a signal to propogate the CloseSend
      // to all running target streams.
      return nil
    }
    if err != nil {
      return err
    }
    select {
    case requestChan <- req:
    case <-ctx.Done():
      return ctx.Err()
    }
  }
}

// dispatch manages incoming requests from `requestChan` by routing them to the supplied stream set
func dispatch(ctx context.Context, requestChan chan *pb.ProxyRequest, replyChan chan *pb.ProxyReply, streamSet *TargetStreamSet) error {

  // Channel to track streams that have completed and should
  // be removed from the stream set.
  doneChan := make(chan uint64)

  for {
    select {
    case <-ctx.Done():
      // Our context has ended.
      return ctx.Err()
    case closedStream := <-doneChan:
      // A stream has closed, and sent its final ServerClose status.
      // Remove it from the active streams list. Further messages
      // received with this stream ID would be a client error.
      streamSet.Remove(closedStream)
    case req, ok := <-requestChan:
      if !ok {
        // The request channel has been closed.
        // This could occur if the proxy client executes
        // a CloseSend(), or Send/Recv() from the client
        // stream has failed with an error.
        // In the latter case, the context cancellation
        // should eventually propagate to the target
        // streams, and cause them to finish.
        // In either case, we should let the target streams
        // know that no further requests will be arriving.
        streamSet.ClientCloseAll()
        streamSet.Wait()
        return ctx.Err()
      }
      // We have a new request.
      switch req.Request.(type) {
      case *pb.ProxyRequest_StartStream:
        streamSet.Add(ctx, req.GetStartStream(), replyChan, doneChan)
      case *pb.ProxyRequest_StreamData:
        if err := streamSet.Send(req.GetStreamData()); err != nil {
          return err
        }
      case *pb.ProxyRequest_ClientCancel:
        if err := streamSet.Cancel(req.GetClientCancel()); err != nil {
          return err
        }
      case *pb.ProxyRequest_ClientClose:
        if err := streamSet.ClientClose(req.GetClientClose()); err != nil {
          return err
        }
      default:
        return status.Errorf(codes.Internal, "unhandled request type %T", req.Request)
      }
    }
  }
}
