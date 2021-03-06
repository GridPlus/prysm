package sync

import (
	"bytes"
	"errors"

	"github.com/libp2p/go-libp2p-core/network"
	"github.com/prysmaticlabs/prysm/beacon-chain/p2p/encoder"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/params"
)

const genericError = "internal service error"
const rateLimitedError = "rate limited"
const stepError = "invalid range or step"

var errWrongForkDigestVersion = errors.New("wrong fork digest version")
var errInvalidEpoch = errors.New("invalid epoch")
var errInvalidFinalizedRoot = errors.New("invalid finalized root")
var errGeneric = errors.New(genericError)

var responseCodeSuccess = byte(0x00)
var responseCodeInvalidRequest = byte(0x01)
var responseCodeServerError = byte(0x02)

func (s *Service) generateErrorResponse(code byte, reason string) ([]byte, error) {
	buf := bytes.NewBuffer([]byte{code})
	resp := &pb.ErrorResponse{
		Message: []byte(reason),
	}
	if _, err := s.p2p.Encoding().EncodeWithMaxLength(buf, resp); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// ReadStatusCode response from a RPC stream.
func ReadStatusCode(stream network.Stream, encoding encoder.NetworkEncoding) (uint8, string, error) {
	// Set ttfb deadline.
	SetStreamReadDeadline(stream, params.BeaconNetworkConfig().TtfbTimeout)
	b := make([]byte, 1)
	_, err := stream.Read(b)
	if err != nil {
		return 0, "", err
	}

	if b[0] == responseCodeSuccess {
		// Set response deadline on a successful response code.
		SetStreamReadDeadline(stream, params.BeaconNetworkConfig().RespTimeout)

		return 0, "", nil
	}

	// Set response deadline, when reading error message.
	SetStreamReadDeadline(stream, params.BeaconNetworkConfig().RespTimeout)
	msg := &pb.ErrorResponse{
		Message: []byte{},
	}
	if err := encoding.DecodeWithMaxLength(stream, msg); err != nil {
		return 0, "", err
	}

	return b[0], string(msg.Message), nil
}

// reads data from the stream without applying any timeouts.
func readStatusCodeNoDeadline(stream network.Stream, encoding encoder.NetworkEncoding) (uint8, string, error) {
	b := make([]byte, 1)
	_, err := stream.Read(b)
	if err != nil {
		return 0, "", err
	}

	if b[0] == responseCodeSuccess {
		return 0, "", nil
	}

	msg := &pb.ErrorResponse{
		Message: []byte{},
	}
	if err := encoding.DecodeWithMaxLength(stream, msg); err != nil {
		return 0, "", err
	}

	return b[0], string(msg.Message), nil
}
