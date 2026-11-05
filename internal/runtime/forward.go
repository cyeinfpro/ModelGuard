package runtime

import (
	"errors"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
	"google.golang.org/grpc"
	"io"
	"net/http"
	"net/url"
	"time"
)

// The host currently binds only OAuth transport plugins. This path implements
// that contract in full; monitoring never changes request bodies or identities.
func (p *Plugin) Forward(stream grpc.BidiStreamingServer[v1.ForwardRequest, v1.ForwardResponse]) error {
	p.forwardStats.begin()
	code := "request_stream_error"
	status := 0
	var firstByte time.Duration
	defer func() { p.forwardStats.finish(code, status, firstByte) }()
	fail := func(errorCode string, sent bool) error {
		code = errorCode
		return stream.Send(&v1.ForwardResponse{Frame: &v1.ForwardResponse_Error{Error: &v1.ForwardResponseError{Code: errorCode, Message: errorCode, RequestSent: sent}}})
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return fail("invalid_start", false)
	}
	u, err := url.Parse(start.Url)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fail("invalid_url", false)
	}
	client, err := p.pool.Client(start.ProxyUrl)
	if err != nil {
		return fail("invalid_proxy", false)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	req, err := http.NewRequestWithContext(stream.Context(), start.Method, start.Url, reader)
	if err != nil {
		return fail("invalid_request", false)
	}
	req.Host = start.Host
	req.ContentLength = start.ContentLength
	for k, v := range start.Headers {
		req.Header[k] = append([]string(nil), v.Values...)
	}
	if !start.HasBody {
		req.Body = nil
		req.ContentLength = 0
	}
	go func() {
		var n int64
		for {
			frame, err := stream.Recv()
			if err != nil {
				writer.CloseWithError(errors.New("request stream interrupted"))
				return
			}
			switch f := frame.Frame.(type) {
			case *v1.ForwardRequest_BodyChunk:
				if !start.HasBody && len(f.BodyChunk) > 0 {
					writer.CloseWithError(errors.New("unexpected body"))
					return
				}
				n += int64(len(f.BodyChunk))
				if n > 256<<20 {
					writer.CloseWithError(errors.New("request too large"))
					return
				}
				if _, err = writer.Write(f.BodyChunk); err != nil {
					return
				}
			case *v1.ForwardRequest_BodyEnd:
				if !f.BodyEnd {
					writer.CloseWithError(errors.New("invalid body end"))
					return
				}
				writer.Close()
				return
			default:
				writer.CloseWithError(errors.New("invalid request frame"))
				return
			}
		}
	}()
	began := time.Now()
	res, err := client.Do(req)
	if err != nil {
		return fail("upstream_transport_error", true)
	}
	defer res.Body.Close()
	status = res.StatusCode
	firstByte = time.Since(began)
	code = "response_stream_error"
	h := map[string]*v1.HeaderValues{}
	for k, v := range res.Header {
		h[k] = &v1.HeaderValues{Values: v}
	}
	if err = stream.Send(&v1.ForwardResponse{Frame: &v1.ForwardResponse_Start{Start: &v1.ForwardResponseStart{StatusCode: int32(res.StatusCode), Status: res.Status, Protocol: res.Proto, ProtocolMajor: int32(res.ProtoMajor), ProtocolMinor: int32(res.ProtoMinor), Headers: h, ContentLength: res.ContentLength}}}); err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			total += int64(n)
			if sendErr := stream.Send(&v1.ForwardResponse{Frame: &v1.ForwardResponse_BodyChunk{BodyChunk: append([]byte(nil), buf[:n]...)}}); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail("upstream_read_error", true)
		}
	}
	err = stream.Send(&v1.ForwardResponse{Frame: &v1.ForwardResponse_End{End: &v1.ForwardResponseEnd{BytesReceived: total, DurationMs: time.Since(began).Milliseconds()}}})
	if err == nil {
		code = ""
	}
	return err
}
