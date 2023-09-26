// Package serverless provides the server serverless function context.
package serverless

import (
	"io"

	"github.com/yomorun/yomo/core"
	"github.com/yomorun/yomo/core/frame"
	"github.com/yomorun/yomo/core/metadata"
	"github.com/yomorun/yomo/pkg/frame-codec/y3codec"
	"golang.org/x/exp/slog"
)

// Context sfn handler context
type Context struct {
	writer    frame.Writer
	dataFrame *frame.DataFrame
}

// NewContext creates a new serverless Context
func NewContext(writer frame.Writer, dataFrame *frame.DataFrame) *Context {
	return &Context{
		writer:    writer,
		dataFrame: dataFrame,
	}
}

// Tag returns the tag of the data frame
func (c *Context) Tag() uint32 {
	return c.dataFrame.Tag
}

// Data returns the data of the data frame
func (c *Context) Data() []byte {
	return c.dataFrame.Payload
}

// Write writes the data
func (c *Context) Write(tag uint32, data []byte) error {
	if data == nil {
		return nil
	}

	dataFrame := &frame.DataFrame{
		Tag:      tag,
		Metadata: c.dataFrame.Metadata,
		Payload:  data,
	}

	return c.writer.WriteFrame(dataFrame)
}

// Streamed returns whether the data is streamed.
func (c *Context) Streamed() bool {
	m, err := metadata.Decode(c.dataFrame.Metadata)
	if err != nil {
		return false
	}
	streamed := core.GetStreamedFromMetadata(m)
	return streamed
}

// Stream returns the stream.
func (c *Context) Stream() io.Reader {
	var streamFrame frame.StreamFrame
	// TODO: codec need to be get from context
	err := y3codec.Codec().Decode(c.Data(), &streamFrame)
	if err != nil {
		slog.Error("[context] StreamFrame decode error", "err", err)
		return nil
	}
	slog.Info("[context] got stream", "stream_frame", streamFrame)
	// TODO: read stream from zipper
	return nil
}
