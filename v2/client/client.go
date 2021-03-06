package client

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/urso/go-lumber/v2/protocol"
)

type Client struct {
	conn net.Conn
	wb   *bytes.Buffer

	opts options
}

type options struct {
	timeout     time.Duration
	encoder     jsonEncoder
	compressLvl int
}

type jsonEncoder func(interface{}) ([]byte, error)

// Option type to be passed to New/Dial functions.
type Option func(*options) error

var (
	codeWindowSize    = []byte{protocol.CodeVersion, protocol.CodeWindowSize}
	codeCompressed    = []byte{protocol.CodeVersion, protocol.CodeCompressed}
	codeJSONDataFrame = []byte{protocol.CodeVersion, protocol.CodeJSONDataFrame}

	empty4 = []byte{0, 0, 0, 0}
)

var (
	// ErrProtocolError is returned if an protocol error was detected in the
	// conversation with lumberjack server.
	ErrProtocolError = errors.New("lumberjack protocol error")
)

// JsonEncoder client option configuring the encoder used to convert events
// to json.
func JSONEncoder(encoder func(interface{}) ([]byte, error)) Option {
	return func(opt *options) error {
		opt.encoder = encoder
		return nil
	}
}

// Timeout client option configuring read/write timeout.
func Timeout(to time.Duration) Option {
	return func(opt *options) error {
		if to < 0 {
			return errors.New("timeouts must not be negative")
		}
		opt.timeout = to
		return nil
	}
}

// CompressionLevel client option setting the compression level (0 to 9)
func CompressionLevel(l int) Option {
	return func(opt *options) error {
		if !(0 <= l && l <= 9) {
			return errors.New("compression level must be within 0 and 9")
		}
		opt.compressLvl = l
		return nil
	}
}

func applyOptions(opts []Option) (options, error) {
	o := options{
		encoder: json.Marshal,
		timeout: 30 * time.Second,
	}

	for _, opt := range opts {
		if err := opt(&o); err != nil {
			return o, err
		}
	}
	return o, nil
}

// Create new client with active connection
func NewWithConn(c net.Conn, opts ...Option) (*Client, error) {
	o, err := applyOptions(opts)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn: c,
		wb:   bytes.NewBuffer(nil),
		opts: o,
	}, nil
}

// Dial up to lumberjack server and return new Client. Returns error
// if connection fails
func Dial(address string, opts ...Option) (*Client, error) {
	o, err := applyOptions(opts)
	if err != nil {
		return nil, err
	}

	dialer := net.Dialer{Timeout: o.timeout}
	return DialWith(dialer.Dial, address, opts...)
}

// DialWith uses provided dialer to connecto to lumberjack server returning a
// new Client. Returns error if connection fails.
func DialWith(
	dial func(network, address string) (net.Conn, error),
	address string,
	opts ...Option,
) (*Client, error) {
	c, err := dial("tcp", address)
	if err != nil {
		return nil, err
	}

	client, err := NewWithConn(c, opts...)
	if err != nil {
		_ = c.Close() // ignore error
		return nil, err
	}
	return client, nil
}

// Close closes underlying network connection
func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) SyncSend(data []interface{}) (int, error) {
	if err := c.Send(data); err != nil {
		return 0, err
	}

	seq, err := c.AwaitACK(uint32(len(data)))
	return int(seq), err
}

// Send sends all data without waiting for ACK
func (c *Client) Send(data []interface{}) error {
	if len(data) == 0 {
		return nil
	}

	// 1. create window message
	c.wb.Reset()
	_, _ = c.wb.Write(codeWindowSize)
	writeUint32(c.wb, uint32(len(data)))

	// 2. serialize data (payload)
	if c.opts.compressLvl > 0 {
		// Compressed Data Frame:
		// version: uint8 = '2'
		// code: uint8 = 'C'
		// payloadSz: uint32
		// payload: compressed payload

		_, _ = c.wb.Write(codeCompressed) // write compressed header

		offSz := c.wb.Len()
		_, _ = c.wb.Write(empty4)
		offPayload := c.wb.Len()

		// compress payload
		w, err := zlib.NewWriterLevel(c.wb, c.opts.compressLvl)
		if err != nil {
			return err
		}

		if err := c.serialize(w, data); err != nil {
			return err
		}

		if err := w.Close(); err != nil {
			return err
		}

		// write compress header
		payloadSz := c.wb.Len() - offPayload
		binary.BigEndian.PutUint32(c.wb.Bytes()[offSz:], uint32(payloadSz))
	} else {
		if err := c.serialize(c.wb, data); err != nil {
			return err
		}
	}

	// 3. send buffer
	if err := c.setWriteDeadline(); err != nil {
		return nil
	}
	payload := c.wb.Bytes()
	for len(payload) > 0 {
		n, err := c.conn.Write(payload)
		if err != nil {
			return err
		}

		payload = payload[n:]
	}

	return nil
}

// ReceiveACK awaits and reads next ACK response or error. Note: Server might
// send partial ACK, in which case client must continue reading ACKs until last send
// window size is matched.
func (c *Client) ReceiveACK() (uint32, error) {
	if err := c.setReadDeadline(); err != nil {
		return 0, err
	}

	var msg [6]byte
	ackbytes := 0
	for ackbytes < 6 {
		n, err := c.conn.Read(msg[ackbytes:])
		if err != nil {
			return 0, err
		}
		ackbytes += n
	}

	// validate response
	isACK := msg[0] == protocol.CodeVersion && msg[1] == protocol.CodeACK
	if !isACK {
		return 0, ErrProtocolError
	}

	seq := binary.BigEndian.Uint32(msg[2:])
	return seq, nil
}

// AwaitACK waits for count elements being ACKed. Returns last known ACK on error.
func (c *Client) AwaitACK(count uint32) (uint32, error) {
	var ackSeq uint32
	var err error

	// read until all acks
	for ackSeq < count {
		ackSeq, err = c.ReceiveACK()
		if err != nil {
			return ackSeq, err
		}
	}

	if ackSeq > count {
		return count, fmt.Errorf(
			"invalid sequence number received (seq=%v, expected=%v)", ackSeq, count)
	}
	return ackSeq, nil
}

func (c *Client) serialize(out io.Writer, data []interface{}) error {
	for i, d := range data {
		b, err := c.opts.encoder(d)
		if err != nil {
			return err
		}

		// Write JSON Data Frame:
		// version: uint8 = '2'
		// code: uint8 = 'J'
		// seq: uint32
		// payloadLen (bytes): uint32
		// payload: JSON document

		_, _ = out.Write(codeJSONDataFrame)
		writeUint32(out, uint32(i))
		writeUint32(out, uint32(len(b)))
		_, _ = out.Write(b)
	}
	return nil
}

func (c *Client) setWriteDeadline() error {
	return c.conn.SetWriteDeadline(time.Now().Add(c.opts.timeout))
}

func (c *Client) setReadDeadline() error {
	return c.conn.SetReadDeadline(time.Now().Add(c.opts.timeout))
}

func writeUint32(out io.Writer, v uint32) {
	_ = binary.Write(out, binary.BigEndian, v)
}
