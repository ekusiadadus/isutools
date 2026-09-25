package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/dbpool"
)

func poolWaitRatio(entry dbpool.Entry) string {
	value, ok := entry.WaitRatio()
	if !ok {
		return "—"
	}
	return humanDuration(value) + " (参考)"
}

func dbPoolSignal(snapshot Snapshot) (bottleneckSignal, bool) {
	if len(snapshot.DBPool) == 0 {
		return bottleneckSignal{}, false
	}
	var waits int64
	var duration time.Duration
	valid, excluded := 0, 0
	var capacity []string
	var churn []string
	for _, entry := range snapshot.DBPool {
		if entry.Partial || entry.Code != "" || entry.WaitCount < 0 || entry.WaitDuration < 0 ||
			entry.MaxIdleClosed < 0 || entry.MaxIdleTimeClosed < 0 || entry.MaxLifetimeClosed < 0 {
			excluded++
			continue
		}
		valid++
		waits += entry.WaitCount
		duration += entry.WaitDuration
		limit := "unlimited"
		if entry.MaxOpen > 0 {
			limit = fmt.Sprint(entry.MaxOpen)
		}
		capacity = append(capacity, fmt.Sprintf("%s open %d / max %s", truncateRunes(entry.TargetID, 100), entry.Open, limit))
		// Connection closes can matter even when database/sql never queues for
		// a connection. Require both a count and an interval-normalized rate so
		// long, low-traffic runs do not receive a noisy warning.
		const minClosures = 100
		const minClosuresPerSecond = 1.0
		closures := entry.MaxIdleClosed + entry.MaxIdleTimeClosed + entry.MaxLifetimeClosed
		interval := entry.Interval()
		if interval > 0 && closures >= minClosures && float64(closures)/interval.Seconds() >= minClosuresPerSecond {
			churn = append(churn, fmt.Sprintf("%s max-idle %d / idle-time %d / lifetime %d (%.1f/s over %s)",
				truncateRunes(entry.TargetID, 100), entry.MaxIdleClosed, entry.MaxIdleTimeClosed,
				entry.MaxLifetimeClosed, float64(closures)/interval.Seconds(), humanDuration(interval)))
		}
	}
	level := "info"
	next := "この区間の待ち開始件数・終了時加算時間はともに0です。進行中の待ちやConn/Txの保持を否定するものではありません。"
	if waits > 0 || duration > 0 {
		level = "warn"
		next = "接続待ちの記録があります。件数は開始時、時間は終了時に増えるため区間境界で対象がずれます。target別のwait・接続保持・DB側負荷・request遅延を照合し、影響を同条件で比較してください。原因や修正順はこの値だけでは確定しません。"
	}
	if len(churn) > 0 {
		level = "warn"
		next += " 接続クローズが多いtargetでは、DB側の接続数・再接続コストと照合し、MaxIdleConns / ConnMaxIdleTime / ConnMaxLifetimeを1条件ずつ変えてscore・passを比較してください。クローズ数だけでは遅延や設定ミスを証明できず、MaxIdleClosedには設定変更時のクローズも含まれます。"
	}
	if excluded > 0 {
		level = "warn"
		next += " partialまたは無効なプールを集計から除外しました。区間を揃えて再計測してください。"
	}
	if valid == 0 {
		next = "有効な区間データがありません。partialまたは無効なプールを診断から除外しました。再計測してください。"
	}
	evidence := fmt.Sprintf("valid pools %d · excluded %d · wait starts %d · completed wait time %s", valid, excluded, waits, humanDuration(duration))
	if len(capacity) > 0 {
		evidence += " · " + strings.Join(capacity, "; ")
	}
	if len(churn) > 0 {
		evidence += " · closure candidates (>=100 and >=1/s): " + strings.Join(churn, "; ")
	}
	return bottleneckSignal{Order: "capacity", Level: level, Signal: "DB pool", Evidence: evidence, NextAction: next}, true
}
