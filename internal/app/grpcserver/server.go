package grpcserver

import (
	"context"
	"net"

	log "github.com/sirupsen/logrus"
	pb "github.com/whereiskurt/meshtk/protos/security/generated"
	"google.golang.org/grpc"
)

// grpcSecurityServer is used to implement meshtasticplugin.MeshtasticPlugin
type grpcSecurityServer struct {
	pb.UnimplementedMeshtasticPluginServer
}

// ModifyPacket implements MeshtasticPlugin.ModifyPacket
func (s *grpcSecurityServer) ModifyPacket(ctx context.Context, req *pb.PacketRequest) (*pb.PacketResponse, error) {
	log.Printf("Received PacketRequest: IP=%s Username=%s ClientID=%s Topic=%s Timestamp=%d",
		req.IpAddress, req.Username, req.ClientId, req.Topic, req.Timestamp)

	// Return the same payload with shouldBlock=false and blockReason="all good"
	return &pb.PacketResponse{
		Payload:     req.Payload,
		ShouldBlock: false,
		BlockReason: "all good",
	}, nil
}

func (n *GrpcServerCmd) StartServer() error {

	address := n.Config.GRpcServer.SecurityAddress

	lis, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}

	s := grpc.NewServer()
	pb.RegisterMeshtasticPluginServer(s, &grpcSecurityServer{})

	n.Config.Log.Infof("MeshtasticPlugin gRPC server listening on %s", address)
	return s.Serve(lis)
}
