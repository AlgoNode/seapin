package serve

import (
	"context"
	"errors"
	"io"
)

// objectReader is the io.ReadSeeker http.ServeContent needs, backed by ranged
// S3 GETs: the first Read after a Seek opens a GET at the current offset, so
// a Range request never downloads the bytes before it.
type objectReader struct {
	ctx  context.Context
	st   Store
	key  string
	size int64
	off  int64
	body io.ReadCloser
	err  error // first storage error, for logging after ServeContent returns
}

func (o *objectReader) Read(p []byte) (int, error) {
	if o.off >= o.size {
		return 0, io.EOF
	}
	if o.body == nil {
		b, err := o.st.GetRange(o.ctx, o.key, o.off)
		if err != nil {
			o.err = err
			return 0, err
		}
		o.body = b
	}
	n, err := o.body.Read(p)
	o.off += int64(n)
	if err != nil && err != io.EOF && o.err == nil {
		o.err = err
	}
	return n, err
}

func (o *objectReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = o.off + offset
	case io.SeekEnd:
		abs = o.size + offset
	default:
		return 0, errors.New("objectReader: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("objectReader: negative position")
	}
	if abs != o.off && o.body != nil {
		o.body.Close()
		o.body = nil
	}
	o.off = abs
	return abs, nil
}

func (o *objectReader) Close() error {
	if o.body == nil {
		return nil
	}
	return o.body.Close()
}
