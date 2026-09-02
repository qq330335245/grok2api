import { Badge } from "@/components/ui/badge";
import { cn } from "@/shared/lib/cn";
import type { BuildDetectAttemptDTO, BuildDetectItemDTO } from "@/features/accounts/accounts-api";
import { useTranslation } from "react-i18next";

function stickyLabel(identity?: string): string {
  const value = identity?.trim() ?? "";
  const plus = value.lastIndexOf("+");
  if (plus >= 0 && plus < value.length - 1) {
    return `+${value.slice(plus + 1)}`;
  }
  return value;
}

function attemptLine(attempt: BuildDetectAttemptDTO, t: (key: string, opts?: Record<string, unknown>) => string): string {
  const verdict = attempt.verdict === "thinking" || attempt.verdict === "missing" || attempt.verdict === "inconclusive"
    ? t(`accounts.detectVerdict.${attempt.verdict}`)
    : "";
  return [
    stickyLabel(attempt.identity),
    attempt.nodeName,
    attempt.exitIp,
    attempt.status ? `HTTP ${attempt.status}` : "",
    verdict,
  ].filter(Boolean).join(" · ");
}

function headline(item: BuildDetectItemDTO, t: (key: string, opts?: Record<string, unknown>) => string): string {
  if (item.outcome === "ok") {
    const event = item.attempts?.find((attempt) => attempt.verdict === "thinking")?.detail
      || item.reason?.replace(/^thinking\s*\((.+)\)\s*$/i, "$1")
      || "";
    return event ? t("accounts.detectBotFlagThinking", { event }) : t("accounts.detectBotFlagThinkingPlain");
  }
  if (item.outcome === "flagged") {
    const count = item.attempts?.length || 0;
    return t("accounts.detectBotFlagNoThinking", { count: count || 2 });
  }
  return item.reason || "";
}

export function DetectResultList({ items, empty, kind = "botflag" }: {
  items: BuildDetectItemDTO[];
  empty: string;
  kind?: "botflag" | "liveness";
}) {
  const { t } = useTranslation();
  if (items.length === 0) {
    return <div className="px-3 py-6 text-center text-sm text-muted-foreground">{empty}</div>;
  }
  return (
    <ul className="divide-y">
      {items.map((item) => {
        const title = item.email && item.name && item.email !== item.name ? item.name : (item.email || item.name || item.id);
        const subtitle = item.email && item.name && item.email !== item.name ? item.email : "";
        const summary = kind === "botflag" ? headline(item, t) : (item.reason || "");
        return (
          <li key={`${item.id}-${item.outcome}-${item.reason ?? ""}`} className="flex items-start gap-3 px-3 py-2.5 text-sm">
            <Badge
              variant="outline"
              className={cn(
                "mt-0.5 shrink-0",
                item.outcome === "ok" && "border-emerald-500/40 text-emerald-700 dark:text-emerald-300",
                item.outcome === "invalid" && "border-destructive/40 text-destructive",
                item.outcome === "flagged" && "border-destructive/40 text-destructive",
                item.outcome === "failed" && "border-amber-500/40 text-amber-700 dark:text-amber-300",
              )}
            >
              {t(`accounts.detectOutcome.${item.outcome}`)}
            </Badge>
            <div className="min-w-0 flex-1">
              <div className="truncate font-medium">{title}</div>
              {subtitle ? <div className="truncate text-xs text-muted-foreground">{subtitle}</div> : null}
              {summary ? <div className="mt-0.5 text-xs text-muted-foreground">{summary}</div> : null}
              {item.attempts?.length ? (
                <div className="mt-1 space-y-0.5 font-mono text-[11px] leading-5 text-muted-foreground">
                  {item.attempts.map((attempt, index) => (
                    <div key={`${attempt.identity ?? "attempt"}-${index}`} className="break-all">
                      {attemptLine(attempt, t) || "—"}
                    </div>
                  ))}
                </div>
              ) : null}
            </div>
          </li>
        );
      })}
    </ul>
  );
}
