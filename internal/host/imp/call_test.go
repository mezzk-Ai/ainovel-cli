package imp

import (
	"context"
	"errors"
	"testing"
)

// TestCallStructuredCarriesRawOnSemanticFailure 守护 §14.2：输出层多次仍非法时，
// 错误必须携带最后一次原始响应，供 runner 统一落 failures/ 失败工件。
func TestCallStructuredCarriesRawOnSemanticFailure(t *testing.T) {
	m := &mockModel{responses: []string{"垃圾输出 not json"}}
	_, err := callStructured[boundaryBatch](context.Background(), m, "sys", "payload", 100, callProfile{}, nil)
	var se *errSemantic
	if !errors.As(err, &se) {
		t.Fatalf("应返回 errSemantic，得 %T：%v", err, err)
	}
	if se.Raw != "垃圾输出 not json" {
		t.Fatalf("Raw 应携带最后一次原始响应，得 %q", se.Raw)
	}
}
