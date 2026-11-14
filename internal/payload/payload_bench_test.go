package payload

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chatBody builds a chat-completions request of roughly size bytes: a long
// conversation plus one large base64 image, as a request with an attachment
// would be.
func chatBody(size int) string {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-6-luna","stream":true,"messages":[`)
	message := `{"role":"user","content":"` + strings.Repeat("lorem ipsum dolor sit amet ", 40) + `"},`
	for b.Len() < size/2 {
		b.WriteString(message)
	}
	b.WriteString(`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", size/2) + `"}}]}]}`)
	return b.String()
}

func BenchmarkRead(b *testing.B) {
	handler := Read(func(http.ResponseWriter, *http.Request) {})
	for _, size := range []int{100 << 10, 1 << 20, 10 << 20} {
		body := chatBody(size)
		b.Run(fmt.Sprintf("%dKB", size>>10), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
			}
		})
	}
}
