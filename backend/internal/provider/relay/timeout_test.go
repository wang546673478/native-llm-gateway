package relay

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wang546673478/native-llm-gateway/internal/database"
)

// TestDefaultTimeout_LocksValue 守卫:中转站兜底超时不许退回 60s。
//
// 60s 是本项目实测踩过的坑:大 body(40k+ token)的**非流式**推理本身就超过 60s,
// 每个候选都在 60s 整点被上游客户端切断,failover 把所有候选试完仍然全 502。
// 流式不受影响(两个 Base 都对流式取 StreamTimeoutFloor=600s 的下限),
// 所以这个值只在非流式路径上可见 —— 也就是最容易被"看着流式没事"而误调小的地方。
func TestDefaultTimeout_LocksValue(t *testing.T) {
	if DefaultTimeout != 400*time.Second {
		t.Errorf("DefaultTimeout = %v, want 400s;调小会让非流式大请求每个候选都被切断(failover 全败)", DefaultTimeout)
	}
}

// TestRelayStationTimeoutTagMatchesDefault 守卫:DB 列 default 与 DefaultTimeout 同源。
//
// 两处分头写死过同一个 60s:Go 兜底(本包)和 GORM 列 default(database.RelayStation)。
// 只改一处的后果是静默的 —— 前端建站时表单总会带上 timeout_seconds,列 default 不生效,
// 于是"看起来对";但任何绕过表单的写入(直接 INSERT / 未来的批量导入)会落回旧值,
// 而它只在非流式大 body 上翻车,极难联想到是列 default。
func TestRelayStationTimeoutTagMatchesDefault(t *testing.T) {
	f, ok := reflect.TypeOf(database.RelayStation{}).FieldByName("Timeout")
	if !ok {
		t.Fatal("database.RelayStation 没有 Timeout 字段 —— 字段被改名了,同步改这个守卫")
	}

	var got string
	for _, part := range strings.Split(f.Tag.Get("gorm"), ";") {
		if v, found := strings.CutPrefix(strings.TrimSpace(part), "default:"); found {
			got = v
			break
		}
	}
	if got == "" {
		t.Fatal("Timeout 的 gorm tag 里没有 default: —— 列 default 丢了,绕过表单的写入会落 0")
	}

	secs, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("gorm default 不是整数秒: %q", got)
	}
	if want := int(DefaultTimeout / time.Second); secs != want {
		t.Errorf("RelayStation.Timeout gorm default = %ds, DefaultTimeout = %ds;两处必须一起改", secs, want)
	}
}
