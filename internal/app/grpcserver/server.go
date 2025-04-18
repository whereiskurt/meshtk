package grpcserver

import (
	"context"
	"log"
	"net"

	pb "github.com/whereiskurt/meshtk/protos/security/generated"

	"google.golang.org/grpc"
)

// server is used to implement meshtasticplugin.MeshtasticPlugin
type server struct {
	pb.UnimplementedMeshtasticPluginServer
}

// ModifyPacket implements MeshtasticPlugin.ModifyPacket
func (s *server) ModifyPacket(ctx context.Context, req *pb.PacketRequest) (*pb.PacketResponse, error) {
	log.Printf("Received PacketRequest: IP=%s Username=%s ClientID=%s Topic=%s Timestamp=%d",
		req.IpAddress, req.Username, req.ClientId, req.Topic, req.Timestamp)

	// Return the same payload with shouldBlock=false and blockReason="all good"
	return &pb.PacketResponse{
		Payload:     req.Payload,
		ShouldBlock: false,
		BlockReason: "all good",
	}, nil
}

// StartServer starts the gRPC server on the specified address
func StartServer(address string) error {
	lis, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}

	s := grpc.NewServer()
	pb.RegisterMeshtasticPluginServer(s, &server{})

	log.Printf("MeshtasticPlugin gRPC server listening on %s", address)
	return s.Serve(lis)
}
