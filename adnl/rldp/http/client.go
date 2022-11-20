package http

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/sigurn/crc16"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/address"
	"github.com/xssnick/tonutils-go/adnl/rldp"
	"github.com/xssnick/tonutils-go/ton/dns"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

const _ChunkSize = 1 << 17
const _RLDPMaxAnswerSize = 2*_ChunkSize + 1024

type DHT interface {
	FindAddresses(ctx context.Context, key []byte) (*address.List, ed25519.PublicKey, error)
}

type Resolver interface {
	Resolve(ctx context.Context, domain string) (*dns.Domain, error)
}

type payloadStream struct {
	Data      []byte
	StartTime time.Time
}

type Transport struct {
	dht      DHT
	resolver Resolver

	rldpClients    map[string]*rldp.RLDP
	activeRequests map[string]*payloadStream
	mx             sync.RWMutex
}

func NewTransport(dht DHT, resolver Resolver) *Transport {
	t := &Transport{
		dht:            dht,
		resolver:       resolver,
		activeRequests: map[string]*payloadStream{},
		rldpClients:    map[string]*rldp.RLDP{},
	}
	return t
}

func (t *Transport) connectRLDP(ctx context.Context, key ed25519.PublicKey, addr, id string) (*rldp.RLDP, error) {
	a, err := adnl.NewADNL(key)
	if err != nil {
		return nil, fmt.Errorf("failed to init adnl for rldp connection %s, err: %w", addr, err)
	}

	if err = a.Connect(ctx, addr); err != nil {
		return nil, fmt.Errorf("failed to connect adnl for rldp connection %s, err: %w", addr, err)
	}

	r := rldp.NewRLDP(a, id)

	// save rldp client to reuse for next requests
	t.mx.Lock()
	t.rldpClients[id] = r
	t.mx.Unlock()

	r.SetOnQuery(t.getRLDPQueryHandler(r))
	r.SetOnDisconnect(func(id string) {
		t.mx.Lock()
		delete(t.rldpClients, id)
		t.mx.Unlock()
	})

	return r, nil
}

func (t *Transport) getRLDPQueryHandler(r *rldp.RLDP) func(query *rldp.Query) error {
	return func(query *rldp.Query) error {
		switch req := query.Data.(type) {
		case GetNextPayloadPart:
			t.mx.Lock()
			stream := t.activeRequests[hex.EncodeToString(req.ID)]
			t.mx.Unlock()

			if stream == nil {
				return fmt.Errorf("unknown request id")
			}

			offset := int(req.Seqno * req.MaxChunkSize)
			if offset >= len(stream.Data) {
				return fmt.Errorf("too big offset for strea data size %d", offset)
			}

			till := len(stream.Data)
			if offset+int(req.MaxChunkSize) < till {
				till = offset + int(req.MaxChunkSize)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := r.SendAnswer(ctx, query.MaxAnswerSize, query.ID, PayloadPart{
				Data:    stream.Data[offset:till],
				Trailer: nil, // TODO: trailer
				IsLast:  till == len(stream.Data),
			})
			cancel()
			if err != nil {
				return fmt.Errorf("failed to send answer: %w", err)
			}

			return nil
		}
		return fmt.Errorf("unexpected query type %s", reflect.TypeOf(query.Data))
	}
}

func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mx.RLock()
	rl := t.rldpClients[request.Host]
	t.mx.RUnlock()

	// TODO: better cache, lock specific domain during resolve
	if rl == nil {
		var err error

		var adnlID []byte
		if strings.HasSuffix(request.Host, ".adnl") {
			adnlID, err = parseADNLAddress(request.Host[:len(request.Host)-5])
			if err != nil {
				return nil, fmt.Errorf("failed to aprse adnl address %s, err: %w", request.Host, err)
			}
		} else {
			var domain *dns.Domain
			for i := 0; i < 3; i++ {
				domain, err = t.resolver.Resolve(request.Context(), request.Host)
				if err != nil {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to resolve host %s, err: %w", request.Host, err)
			}

			adnlID = domain.GetSiteRecord()
		}

		addresses, pubKey, err := t.dht.FindAddresses(request.Context(), adnlID)
		if err != nil {
			return nil, fmt.Errorf("failed to find address of %s (%s) in DHT, err: %w", request.Host, hex.EncodeToString(adnlID), err)
		}

		var triedAddresses []string
		for _, v := range addresses.Addresses {
			addr := fmt.Sprintf("%s:%d", v.IP.String(), v.Port)
			// find working rld node addr
			rl, err = t.connectRLDP(request.Context(), pubKey, addr, request.Host)
			if err == nil {
				break
			}

			triedAddresses = append(triedAddresses, addr)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to connect to rldp servers %s of host %s, err: %w", triedAddresses, request.Host, err)
		}
	}

	// TODO: async stream body for req and resp
	if request.Body != nil {
		defer request.Body.Close()
	}

	qid := make([]byte, 32)
	_, err := rand.Read(qid)
	if err != nil {
		return nil, err
	}

	req := Request{
		ID:      qid,
		Method:  request.Method,
		URL:     request.URL.String(),
		Version: "HTTP/1.1",
		Headers: []Header{
			{
				Name:  "Host",
				Value: request.Host,
			},
		},
	}

	if request.ContentLength > 0 {
		req.Headers = append(req.Headers, Header{
			Name:  "Content-Length",
			Value: fmt.Sprint(request.ContentLength),
		})
	}

	for k, v := range request.Header {
		for _, hdr := range v {
			req.Headers = append(req.Headers, Header{
				Name:  k,
				Value: hdr,
			})
		}
	}

	if request.Body != nil {
		// TODO: stream
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read request body: %w", err)
		}

		t.mx.Lock()
		t.activeRequests[hex.EncodeToString(qid)] = &payloadStream{
			Data:      data,
			StartTime: time.Now(),
		}
		t.mx.Unlock()
	}

	var res Response
	err = rl.DoQuery(request.Context(), _RLDPMaxAnswerSize, req, &res)
	if err != nil {
		return nil, fmt.Errorf("failed to query http over rldp: %w", err)
	}

	t.mx.Lock()
	delete(t.activeRequests, hex.EncodeToString(qid))
	t.mx.Unlock()

	httpResp := &http.Response{
		Status:        res.Reason,
		StatusCode:    int(res.StatusCode),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        map[string][]string{},
		ContentLength: -1,
		Trailer:       map[string][]string{},
		Request:       request,
	}

	for _, header := range res.Headers {
		httpResp.Header[header.Name] = []string{header.Value}
	}

	if ln, ok := request.Header["Content-Length"]; ok && len(ln) > 0 {
		httpResp.ContentLength, err = strconv.ParseInt(ln[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse content length: %w", err)
		}
	}

	var buf []byte
	if httpResp.ContentLength > 0 {
		buf = make([]byte, 0, httpResp.ContentLength)

		seqno := int32(0)
		for !res.NoPayload {
			var part PayloadPart
			err = rl.DoQuery(request.Context(), _RLDPMaxAnswerSize, GetNextPayloadPart{
				ID:           qid,
				Seqno:        seqno,
				MaxChunkSize: _ChunkSize,
			}, &part)
			if err != nil {
				return nil, fmt.Errorf("failed to query rldp response part %d: %w", seqno, err)
			}

			for _, tr := range part.Trailer {
				httpResp.Trailer[tr.Name] = []string{tr.Value}
			}

			res.NoPayload = part.IsLast
			buf = append(buf, part.Data...)
			seqno++
		}
	}

	httpResp.Body = io.NopCloser(bytes.NewBuffer(buf))

	return httpResp, nil
}

func parseADNLAddress(addr string) ([]byte, error) {
	if len(addr) != 55 {
		return nil, errors.New("wrong id length")
	}

	buf, err := base32.StdEncoding.DecodeString("F" + strings.ToUpper(addr))
	if err != nil {
		return nil, fmt.Errorf("failed to decode address: %w", err)
	}

	if buf[0] != 0x2d {
		return nil, errors.New("invalid first byte")
	}

	hash := binary.BigEndian.Uint16(buf[33:])
	calc := crc16.Checksum(buf[:33], crc16.MakeTable(crc16.CRC16_XMODEM))
	if hash != calc {
		return nil, errors.New("invalid address")
	}

	return buf[:32], nil
}

/*
td::Result<Bits256> adnl_id_decode(td::Slice id) {
  if (id.size() != 55) {
    return td::Status::Error("Wrong length of adnl id");
  }
  td::uint8 buf[56];
  buf[0] = 'f';
  td::MutableSlice buf_slice(buf, 56);
  buf_slice.substr(1).copy_from(id);
  TRY_RESULT(decoded_str, td::base32_decode(buf_slice));
  auto decoded = td::Slice(decoded_str);
  if (decoded[0] != 0x2d) {
    return td::Status::Error("Invalid first byte");
  }
  auto got_hash = (decoded.ubegin()[33] << 8) | decoded.ubegin()[34];
  auto hash = td::crc16(decoded.substr(0, 33));
  if (hash != got_hash) {
    return td::Status::Error("Hash mismatch");
  }
  Bits256 res;
  res.as_slice().copy_from(decoded.substr(1, 32));
  return res;
}
*/
