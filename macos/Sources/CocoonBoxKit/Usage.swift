import Foundation

extension Cocoonbox_Control_V1_UsageResponse {
    public var object: [String: Any] {
        [
            "today": Self.object(today), "month": Self.object(month), "daily_cap_micros": dailyCapMicros, "default_cap": defaultCap,
            "remaining_today_micros": remainingTodayMicros,
        ]
    }

    private static func object(_ p: Cocoonbox_Control_V1_UsagePeriod) -> [String: Any] {
        [
            "since_ms": p.sinceMs, "requests": p.requests, "prompt_tokens": p.promptTokens, "completion_tokens": p.completionTokens,
            "total_tokens": p.totalTokens, "cost_micros": p.costMicros,
            "models": p.models.map {
                [
                    "model": $0.model, "requests": $0.requests, "prompt_tokens": $0.promptTokens, "completion_tokens": $0.completionTokens,
                    "total_tokens": $0.totalTokens, "cost_micros": $0.costMicros,
                ] as [String: Any]
            },
        ]
    }
}
