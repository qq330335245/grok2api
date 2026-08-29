import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Activity, ChevronDown, Network, Pencil, RefreshCw, ShieldCheck, ShieldX, TimerReset } from "lucide-react";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate } from "react-router-dom";
import { toast } from "sonner";

import { ApiError } from "@/shared/api/client";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { detectBuildBotFlags, updateAccountsEnabled, type BuildDetectItemDTO } from "@/features/accounts/accounts-api";
import { clearAntiDegradeAccount, clearAntiDegradeIP, getAntiDegradeStatus, updateAntiDegradeConfig, type AntiDegradeConfigDTO, type AntiDegradeIPDTO, type AntiDegradeQuarantineDTO, type AntiDegradeStatusDTO } from "@/features/antidegrade/antidegrade-api";
import { ErrorState, LoadingState } from "@/shared/components/data-state";
import { PageHeader } from "@/shared/components/page-header";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";

const ANTI_DEGRADE_PROVIDERS = ["grok_build", "grok_console", "grok_web"] as const;

function withProviders(config: AntiDegradeConfigDTO): AntiDegradeConfigDTO {
  const providers = (config.providers ?? []).filter((item) => ANTI_DEGRADE_PROVIDERS.includes(item as (typeof ANTI_DEGRADE_PROVIDERS)[number]));
  return { ...config, providers: providers.length ? providers : ["grok_build"] };
}

export function AntidegradePage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [form, setForm] = useState<AntiDegradeConfigDTO | null>(null);
  const statusQuery = useQuery({
    queryKey: ["antidegrade"],
    queryFn: getAntiDegradeStatus,
    refetchInterval: 5_000,
  });

  const applyStatus = (status: AntiDegradeStatusDTO) => {
    queryClient.setQueryData(["antidegrade"], status);
    setForm(withProviders(status.config));
  };
  const clearIP = useMutation({
    mutationFn: clearAntiDegradeIP,
    onSuccess: (status) => { applyStatus(status); toast.success(t("antidegrade.clearedIp")); },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("common.retry")),
  });
  const saveConfig = useMutation({
    mutationFn: (config: AntiDegradeConfigDTO) => {
      if (!statusQuery.data) throw new Error("missing form");
      return updateAntiDegradeConfig(statusQuery.data.revision, withProviders(config));
    },
    onSuccess: (status) => { applyStatus(status); setSettingsOpen(false); toast.success(t("antidegrade.saved")); },
    onError: (error) => {
      if (error instanceof ApiError && error.status === 409) {
        toast.error(t("antidegrade.conflict"));
        void statusQuery.refetch().then((result) => { if (result.data) setForm(withProviders(result.data.config)); });
        return;
      }
      toast.error(error instanceof Error ? error.message : t("common.retry"));
    },
  });

  const status = statusQuery.data;
  const openSettings = () => {
    if (status) setForm(withProviders(status.config));
    setSettingsOpen(true);
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title={t("antidegrade.title")}
        description={t("antidegrade.description")}
        actions={(
          <Button variant="secondary" size="sm" onClick={() => void statusQuery.refetch()} disabled={statusQuery.isFetching}>
            <RefreshCw className={cn(statusQuery.isFetching && "animate-spin")} />
            {t("common.refresh")}
          </Button>
        )}
      />
      {statusQuery.isError ? <ErrorState message={statusQuery.error instanceof Error ? statusQuery.error.message : t("common.retry")} onRetry={() => void statusQuery.refetch()} /> : null}
      {statusQuery.isLoading && !status ? <LoadingState /> : null}
      {status ? (
        <>
          <Overview status={status} />
          <ScopeCard
            providers={withProviders(status.config).providers}
            saving={saveConfig.isPending}
            onChange={(providers) => saveConfig.mutate({ ...withProviders(status.config), providers })}
          />
          <IPLoadPanel ips={status.ips} locale={i18n.language} onClear={(exitIp) => clearIP.mutate(exitIp)} clearing={clearIP.isPending} />
          <div className="grid gap-3 xl:grid-cols-[minmax(0,3fr)_minmax(300px,2fr)]">
            <QuarantinePanel status={status} locale={i18n.language} onStatus={applyStatus} />
            <div className="space-y-3">
              <PolicyCard config={withProviders(status.config)} onEdit={openSettings} />
              <EventPanel status={status} locale={i18n.language} />
            </div>
          </div>
          <Dialog open={settingsOpen} onOpenChange={setSettingsOpen}>
            <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-2xl">
              <DialogHeader>
                <DialogTitle>{t("antidegrade.settingsTab")}</DialogTitle>
                <DialogDescription>{t("antidegrade.saveHint")}</DialogDescription>
              </DialogHeader>
              {form ? (
                <SettingsForm form={form} setForm={setForm} saving={saveConfig.isPending} onSave={() => saveConfig.mutate(form)} onCancel={() => setSettingsOpen(false)} />
              ) : null}
            </DialogContent>
          </Dialog>
        </>
      ) : null}
    </div>
  );
}

function Overview({ status }: { status: AntiDegradeStatusDTO }) {
  const { t } = useTranslation();
  const config = withProviders(status.config);
  const cooling = status.ips.filter((ip) => ip.cooling).length;
  const channels = config.providers.map((provider) => t(`antidegrade.providerNames.${provider}`)).join(" / ");
  return (
    <section className="grid overflow-hidden rounded-lg bg-card sm:grid-cols-2 xl:grid-cols-4" aria-label={t("antidegrade.overview")}>
      <Metric
        icon={config.enabled ? ShieldCheck : ShieldX}
        label={t("antidegrade.serviceStatus")}
        value={config.enabled ? t("antidegrade.running") : t("antidegrade.stopped")}
        detail={t(`antidegrade.modes.${config.mode}`)}
        tone={config.enabled ? "good" : "bad"}
      />
      <Metric icon={Network} label={t("antidegrade.providers")} value={channels || t("antidegrade.providerNames.grok_build")} detail={t("antidegrade.scopeHelp")} />
      <Metric icon={Activity} label={t("antidegrade.cooling")} value={String(cooling)} detail={t("antidegrade.coolingHelp")} tone={cooling ? "bad" : "good"} />
      <Metric icon={TimerReset} label={t("antidegrade.quarantined")} value={String(status.quarantined.length)} detail={t("antidegrade.quarantineHelp")} tone={status.quarantined.length ? "bad" : "good"} />
    </section>
  );
}

function Metric({ icon: Icon, label, value, detail, tone }: { icon: typeof ShieldCheck; label: string; value: string; detail: string; tone?: "good" | "bad" }) {
  return (
    <div className="flex min-h-24 items-center gap-3 border-b p-4 last:border-b-0 sm:[&:nth-child(odd)]:border-r xl:border-b-0 xl:border-r xl:last:border-r-0">
      <span className={cn("flex size-9 shrink-0 items-center justify-center rounded-md bg-secondary text-muted-foreground", tone === "good" && "text-emerald-600 dark:text-emerald-400", tone === "bad" && "text-destructive")}>
        <Icon className="size-4" />
      </span>
      <div className="min-w-0">
        <p className="text-xs text-muted-foreground">{label}</p>
        <p className="mt-1 truncate text-lg font-medium tabular-nums">{value}</p>
        <p className="mt-1 truncate text-[11px] text-muted-foreground">{detail}</p>
      </div>
    </div>
  );
}

function ScopeCard({
  providers, saving, onChange,
}: {
  providers: string[];
  saving: boolean;
  onChange: (providers: string[]) => void;
}) {
  const { t } = useTranslation();
  const toggle = (provider: string, checked: boolean) => {
    const next = checked ? [...new Set([...providers, provider])] : providers.filter((item) => item !== provider);
    onChange(next.length ? next : ["grok_build"]);
  };
  return (
    <section className="flex flex-wrap items-center gap-x-5 gap-y-2 rounded-lg bg-card px-4 py-2.5 sm:px-5">
      <div className="min-w-0">
        <h2 className="text-sm font-medium">{t("antidegrade.providers")}</h2>
        <p className="text-[11px] text-muted-foreground">{t("antidegrade.providersHelp")}</p>
      </div>
      <div className="ml-auto flex flex-wrap items-center gap-x-4 gap-y-1.5">
        {ANTI_DEGRADE_PROVIDERS.map((provider) => {
          const checked = providers.includes(provider);
          return (
            <label key={provider} className="flex items-center gap-2 text-xs">
              <span>{t(`antidegrade.providerNames.${provider}`)}</span>
              <Switch checked={checked} disabled={saving} onCheckedChange={(value) => toggle(provider, value)} />
            </label>
          );
        })}
      </div>
    </section>
  );
}

function PolicyCard({ config, onEdit }: { config: AntiDegradeConfigDTO; onEdit: () => void }) {
  const { t } = useTranslation();
  const rows = [
    [t("antidegrade.mode"), t(`antidegrade.modes.${config.mode}`)],
    [t("antidegrade.densityWindow"), config.densityWindow],
    [t("antidegrade.densityMaxAccounts"), String(config.densityMaxAccounts)],
    [t("antidegrade.failExitThreshold"), String(config.failExitThreshold)],
    [t("antidegrade.dirtyIpCooldown"), config.dirtyIpCooldown],
    [t("antidegrade.maxIpRetries"), String(config.maxIpRetries)],
  ];
  return (
    <section className="overflow-hidden rounded-lg bg-card p-4 sm:p-5">
      <div className="flex items-center justify-between gap-3">
        <h2 className="text-sm font-medium">{t("antidegrade.policy")}</h2>
        <Button type="button" variant="ghost" size="sm" onClick={onEdit}><Pencil />{t("antidegrade.editPolicy")}</Button>
      </div>
      <dl className="mt-4 grid grid-cols-2 gap-x-5 gap-y-4">
        {rows.map(([label, value]) => (
          <div key={label}>
            <dt className="text-[11px] text-muted-foreground">{label}</dt>
            <dd className="mt-1 text-sm font-medium tabular-nums">{value}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

type LoadFilter = "all" | "cooling" | "full" | "ok";

function isFullIP(ip: AntiDegradeIPDTO) {
  return !ip.cooling && ip.accountCount >= Math.max(ip.accountLimit, 1);
}

function isStickyIP(ip: AntiDegradeIPDTO) {
  return ip.exitIp.startsWith("account:") || ip.exitIp.startsWith("node:");
}

function nodeGroupKey(ip: AntiDegradeIPDTO) {
  if (ip.nodeIds[0]) return `node:${ip.nodeIds[0]}`;
  if (ip.nodeNames[0]) return `name:${ip.nodeNames[0]}`;
  return `ip:${ip.exitIp}`;
}

function nodeGroupTitle(ip: AntiDegradeIPDTO, fallback: string) {
  return ip.nodeNames[0] || (ip.nodeIds[0] ? `#${ip.nodeIds[0]}` : fallback);
}

function matchesLoadFilter(ip: AntiDegradeIPDTO, filter: LoadFilter) {
  if (filter === "cooling") return ip.cooling;
  if (filter === "full") return isFullIP(ip);
  if (filter === "ok") return !ip.cooling && !isFullIP(ip);
  return true;
}

function ipStatusLabel(ip: AntiDegradeIPDTO, t: (key: string) => string) {
  if (ip.operatorOverrideUntil) return t("antidegrade.override");
  if (ip.cooling) return t("antidegrade.cooling");
  if (isFullIP(ip)) return t("antidegrade.full");
  if (ip.accountCount / Math.max(ip.accountLimit, 1) >= 0.6) return t("antidegrade.nearFull");
  return t("antidegrade.idle");
}

function groupStickyIPs(sticky: AntiDegradeIPDTO[], fallbackTitle: string) {
  const groups = new Map<string, { key: string; title: string; items: AntiDegradeIPDTO[] }>();
  for (const ip of sticky) {
    const key = nodeGroupKey(ip);
    const current = groups.get(key);
    if (current) {
      current.items.push(ip);
      continue;
    }
    groups.set(key, { key, title: nodeGroupTitle(ip, fallbackTitle), items: [ip] });
  }
  return [...groups.values()].sort((a, b) => {
    const aHot = a.items.some((ip) => ip.cooling || isFullIP(ip)) ? 0 : 1;
    const bHot = b.items.some((ip) => ip.cooling || isFullIP(ip)) ? 0 : 1;
    if (aHot !== bHot) return aHot - bHot;
    return a.title.localeCompare(b.title);
  });
}

function ipStatusClass(ip: AntiDegradeIPDTO) {
  if (ip.cooling || ip.operatorOverrideUntil) return "text-muted-foreground";
  if (isFullIP(ip)) return "text-destructive";
  if (ip.accountCount / Math.max(ip.accountLimit, 1) >= 0.6) return "text-amber-700 dark:text-amber-400";
  return "text-emerald-600 dark:text-emerald-400";
}

function IPLoadPanel({ ips, locale, onClear, clearing }: { ips: AntiDegradeIPDTO[]; locale: string; onClear: (exitIp: string) => void; clearing: boolean }) {
  const { t } = useTranslation();
  const [filter, setFilter] = useState<LoadFilter>("all");
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const coolingCount = ips.filter((ip) => ip.cooling).length;
  const fullCount = ips.filter((ip) => isFullIP(ip)).length;
  const okCount = ips.length - coolingCount - fullCount;
  const visible = ips.filter((ip) => matchesLoadFilter(ip, filter));
  const sticky = visible.filter(isStickyIP);
  const dedicated = visible.filter((ip) => !isStickyIP(ip));
  const stickyGroups = groupStickyIPs(sticky, t("antidegrade.nodes"));
  const dedicatedSorted = [...dedicated].sort((a, b) => {
    const aHot = a.cooling || isFullIP(a) ? 0 : 1;
    const bHot = b.cooling || isFullIP(b) ? 0 : 1;
    if (aHot !== bHot) return aHot - bHot;
    return a.exitIp.localeCompare(b.exitIp);
  });
  const filters: { id: LoadFilter; label: string; count: number }[] = [
    { id: "all", label: t("antidegrade.filterAll"), count: ips.length },
    { id: "cooling", label: t("antidegrade.cooling"), count: coolingCount },
    { id: "full", label: t("antidegrade.full"), count: fullCount },
    { id: "ok", label: t("antidegrade.filterOk"), count: okCount },
  ];
  return (
    <section className="overflow-hidden rounded-lg bg-card">
      <div className="flex flex-wrap items-start justify-between gap-3 border-b px-4 py-3 sm:px-5">
        <div className="min-w-0">
          <h2 className="text-sm font-medium">{t("antidegrade.loadTab")}</h2>
          <p className="mt-1 text-xs text-muted-foreground">{t("antidegrade.loadHelp")}</p>
        </div>
        <div className="flex flex-wrap gap-1">
          {filters.map((item) => (
            <button
              key={item.id}
              type="button"
              onClick={() => setFilter(item.id)}
              className={cn("rounded-md px-2 py-1 text-[11px] tabular-nums", filter === item.id ? "bg-secondary font-medium" : "text-muted-foreground hover:bg-secondary/60")}
            >
              {item.label} {item.count}
            </button>
          ))}
        </div>
      </div>
      {visible.length === 0 ? (
        <p className="px-4 py-10 text-center text-xs text-muted-foreground sm:px-5">{t("antidegrade.noIps")}</p>
      ) : (
        <div className="max-h-[28rem] overflow-y-auto">
          {stickyGroups.map((group) => {
            const hot = group.items.some((ip) => ip.cooling || isFullIP(ip));
            const open = expanded[group.key] ?? hot;
            const cooling = group.items.filter((ip) => ip.cooling).length;
            return (
              <div key={group.key} className="border-b last:border-b-0">
                <button type="button" className="flex w-full items-center gap-2 px-4 py-2.5 text-left sm:px-5" onClick={() => setExpanded((current) => ({ ...current, [group.key]: !open }))}>
                  <ChevronDown className={cn("size-3.5 shrink-0 text-muted-foreground transition", !open && "-rotate-90")} />
                  <span className="min-w-0 flex-1 truncate text-sm font-medium">{group.title}</span>
                  <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground">
                    {t("antidegrade.groupSummary", { count: group.items.length, cooling })}
                  </span>
                </button>
                {open ? group.items.map((ip) => (
                  <CompactIPRow key={ip.exitIp} ip={ip} locale={locale} sticky clearing={clearing} onClear={() => onClear(ip.exitIp)} />
                )) : null}
              </div>
            );
          })}
          {dedicatedSorted.length > 0 ? (
            <div className={cn(stickyGroups.length > 0 && "border-t")}>
              {stickyGroups.length > 0 ? (
                <p className="px-4 py-2.5 text-[11px] text-muted-foreground sm:px-5">{t("antidegrade.fixedExits")}</p>
              ) : null}
              {dedicatedSorted.map((ip) => (
                <CompactIPRow key={ip.exitIp} ip={ip} locale={locale} sticky={false} clearing={clearing} onClear={() => onClear(ip.exitIp)} />
              ))}
            </div>
          ) : null}
        </div>
      )}
    </section>
  );
}

function CompactIPRow({ ip, locale, sticky, onClear, clearing }: { ip: AntiDegradeIPDTO; locale: string; sticky: boolean; onClear: () => void; clearing: boolean }) {
  const { t } = useTranslation();
  const primary = sticky
    ? (ip.accounts[0]?.name || ip.accounts[0]?.id || ip.exitIp)
    : ip.exitIp;
  const secondary = sticky
    ? ip.accounts.slice(1).map((account) => account.name || account.id).join(" · ")
    : (ip.nodeNames[0] || (ip.nodeIds[0] ? `#${ip.nodeIds[0]}` : ""));
  const extra = !sticky && ip.accounts.length > 0
    ? ip.accounts.slice(0, 2).map((account) => account.name || account.id).join(" · ") + (ip.accounts.length > 2 ? ` +${ip.accounts.length - 2}` : "")
    : "";
  const reason = ip.cooldownReason ? t(`antidegrade.reasons.${ip.cooldownReason}`, { defaultValue: ip.cooldownReason }) : "";
  const limit = Math.max(ip.accountLimit, 1);
  const ratio = Math.min(ip.accountCount / limit, 1);
  const barTone = ip.cooling ? "bg-muted-foreground/40" : isFullIP(ip) ? "bg-destructive" : ratio >= 0.6 ? "bg-amber-500" : "bg-emerald-500";
  return (
    <div className="flex items-center gap-3 border-t px-4 py-2.5 sm:px-5">
      <span className={cn("size-1.5 shrink-0 rounded-full", ip.cooling ? "bg-muted-foreground/50" : isFullIP(ip) ? "bg-destructive" : "bg-emerald-500")} />
      <div className="min-w-0 flex-[1.2]">
        <p className="truncate font-mono text-xs">{primary}</p>
        {secondary || extra ? (
          <p className="mt-0.5 truncate text-[11px] text-muted-foreground">{[secondary, extra].filter(Boolean).join(" · ")}</p>
        ) : null}
        {ip.cooling && ip.cooldownUntil ? (
          <p className="mt-0.5 truncate text-[11px] text-muted-foreground">{reason} · {t("antidegrade.until", { time: formatDateTime(ip.cooldownUntil, locale) })}</p>
        ) : null}
      </div>
      <div className="flex min-w-28 flex-1 items-center gap-2">
        <div className="h-1.5 min-w-16 flex-1 overflow-hidden rounded-full bg-muted">
          <i className={cn("block h-full", barTone)} style={{ width: `${Math.max(ratio * 100, ip.accountCount > 0 ? 8 : 0)}%` }} />
        </div>
        <span className="shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">{ip.accountCount}/{ip.accountLimit}</span>
      </div>
      <span className={cn("w-12 shrink-0 text-right text-[11px]", ipStatusClass(ip))}>{ipStatusLabel(ip, t)}</span>
      {ip.cooling ? (
        <Button size="sm" variant="ghost" className="h-7 px-2" disabled={clearing} onClick={onClear}>{t("antidegrade.clearIp")}</Button>
      ) : <span className="w-12 shrink-0" />}
    </div>
  );
}

function QuarantinePanel({ status, locale, onStatus }: { status: AntiDegradeStatusDTO; locale: string; onStatus: (status: AntiDegradeStatusDTO) => void }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [detectOpen, setDetectOpen] = useState(false);
  const [detectItems, setDetectItems] = useState<BuildDetectItemDTO[]>([]);
  const ids = status.quarantined.map((account) => account.id);
  const selectedIds = ids.filter((id) => selected.has(id));
  const allSelected = ids.length > 0 && selectedIds.length === ids.length;
  const clearOne = useMutation({
    mutationFn: clearAntiDegradeAccount,
    onSuccess: (next) => { onStatus(next); toast.success(t("antidegrade.clearedAccount")); },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("common.retry")),
  });
  const clearMany = useMutation({
    mutationFn: async (accountIds: string[]) => {
      let latest = status;
      for (const id of accountIds) latest = await clearAntiDegradeAccount(id);
      return latest;
    },
    onSuccess: (next) => {
      onStatus(next);
      setSelected(new Set());
      toast.success(t("antidegrade.clearedAccounts", { count: selectedIds.length }));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("common.retry")),
  });
  const disableMany = useMutation({
    mutationFn: (accountIds: string[]) => updateAccountsEnabled(accountIds, false, "grok_build"),
    onSuccess: (result) => {
      setSelected(new Set());
      toast.success(t("antidegrade.disabledAccounts", { count: result.updated }));
      void getAntiDegradeStatus().then(onStatus);
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("common.retry")),
  });
  const detectMany = useMutation({
    mutationFn: (accountIds: string[]) => {
      setDetectItems([]);
      setDetectOpen(true);
      return detectBuildBotFlags({ ids: accountIds }, {
        onItem: (item) => setDetectItems((current) => [...current, item].slice(-50)),
      });
    },
    onSuccess: (result) => toast.success(t("accounts.batchBotFlagsDetected", { succeeded: result.succeeded, failed: result.failed })),
    onError: (error) => toast.error(error instanceof Error ? error.message : t("common.retry")),
  });
  const pending = clearOne.isPending || clearMany.isPending || disableMany.isPending || detectMany.isPending;
  const toggle = (id: string, checked: boolean) => {
    setSelected((current) => {
      const next = new Set(current);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  };
  const targets = selectedIds;
  const openVideo = (accountIds: string[]) => {
    const id = accountIds[0];
    if (!id) return;
    navigate(`/creative-console?video=1&account=${encodeURIComponent(id)}`);
    toast.success(t("antidegrade.testVideoOpen", { count: accountIds.length }));
  };
  return (
    <section className="overflow-hidden rounded-lg bg-card">
      <div className="border-b px-4 py-3 sm:px-5">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="min-w-0">
            <h2 className="text-sm font-medium">{t("antidegrade.quarantined")}</h2>
            <p className="mt-1 text-xs text-muted-foreground">{t("antidegrade.quarantineHelp")}</p>
          </div>
          {selectedIds.length > 0 ? (
            <div className="flex flex-wrap items-center gap-1.5">
              <span className="text-[11px] text-muted-foreground">{t("common.selectedCount", { count: selectedIds.length })}</span>
              <Button size="sm" variant="secondary" disabled={pending} onClick={() => detectMany.mutate(targets)}>{t("antidegrade.detectBotFlag")}</Button>
              <Button size="sm" variant="secondary" disabled={pending} onClick={() => openVideo(targets)}>{t("antidegrade.testVideo")}</Button>
              <Button size="sm" variant="secondary" disabled={pending} onClick={() => disableMany.mutate(targets)}>{t("common.disable")}</Button>
              <Button size="sm" variant="secondary" disabled={pending} onClick={() => clearMany.mutate(targets)}>{t("antidegrade.clearAccount")}</Button>
            </div>
          ) : null}
        </div>
      </div>
      {status.quarantined.length === 0 ? (
        <p className="px-4 py-10 text-center text-xs text-muted-foreground sm:px-5">{t("antidegrade.noQuarantine")}</p>
      ) : (
        <Table viewportRows={8} rowHeight={44}>
          <TableHeader>
            <TableRow>
              <TableHead className="w-10">
                <Checkbox
                  checked={allSelected ? true : selectedIds.length > 0 ? "indeterminate" : false}
                  onCheckedChange={(value) => setSelected(value === true ? new Set(ids) : new Set())}
                  aria-label={t("common.selectPage")}
                />
              </TableHead>
              <TableHead>{t("antidegrade.accounts")}</TableHead>
              <TableHead>{t("antidegrade.failedIps")}</TableHead>
              <TableHead>{t("antidegrade.recidivism")}</TableHead>
              <TableHead>{t("antidegrade.cooldown")}</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {status.quarantined.map((account) => (
              <QuarantineRow
                key={account.id}
                account={account}
                locale={locale}
                checked={selected.has(account.id)}
                pending={pending}
                onCheckedChange={(checked) => toggle(account.id, checked)}
                onDetect={() => detectMany.mutate([account.id])}
                onVideo={() => openVideo([account.id])}
                onDisable={() => disableMany.mutate([account.id])}
                onClear={() => clearOne.mutate(account.id)}
              />
            ))}
          </TableBody>
        </Table>
      )}
      <Dialog open={detectOpen} onOpenChange={setDetectOpen}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t("antidegrade.detectBotFlag")}</DialogTitle>
            <DialogDescription>{t("accounts.detectBotFlagSelectedDescription", { count: selectedIds.length || detectItems.length || 1 })}</DialogDescription>
          </DialogHeader>
          <div className="max-h-64 overflow-y-auto rounded-md border">
            {detectItems.length === 0 ? (
              <p className="px-3 py-6 text-center text-sm text-muted-foreground">{detectMany.isPending ? t("accounts.detectWaitingResults") : t("accounts.detectNoResults")}</p>
            ) : detectItems.map((item) => (
              <div key={`${item.id}-${item.outcome}-${item.reason ?? ""}`} className="border-b px-3 py-2 last:border-b-0">
                <p className="text-sm">{item.name || item.id} · {t(`accounts.detectOutcome.${item.outcome}`)}</p>
                {item.reason ? <p className="mt-0.5 break-all text-[11px] text-muted-foreground">{item.reason}</p> : null}
                {item.attempts?.length ? (
                  <div className="mt-1 space-y-0.5 font-mono text-[11px] text-muted-foreground">
                    {item.attempts.map((attempt, index) => (
                      <div key={`${attempt.identity ?? "attempt"}-${index}`} className="break-all">
                        {[attempt.identity, attempt.nodeName, attempt.exitIp, attempt.status ? `HTTP ${attempt.status}` : "", attempt.detail].filter(Boolean).join(" · ") || "—"}
                      </div>
                    ))}
                  </div>
                ) : null}
              </div>
            ))}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDetectOpen(false)}>{t("common.close")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  );
}

function QuarantineRow({
  account, locale, checked, pending, onCheckedChange, onDetect, onVideo, onDisable, onClear,
}: {
  account: AntiDegradeQuarantineDTO;
  locale: string;
  checked: boolean;
  pending: boolean;
  onCheckedChange: (checked: boolean) => void;
  onDetect: () => void;
  onVideo: () => void;
  onDisable: () => void;
  onClear: () => void;
}) {
  const { t } = useTranslation();
  return (
    <TableRow>
      <TableCell className="w-10">
        <Checkbox checked={checked} onCheckedChange={(value) => onCheckedChange(value === true)} aria-label={t("common.selectItem", { name: account.name || account.id })} />
      </TableCell>
      <TableCell>
        <Link className="text-sm hover:underline" to="/accounts">{account.name || account.id}</Link>
      </TableCell>
      <TableCell className="max-w-40 truncate font-mono text-xs">{account.failedExitIps.join(", ") || "—"}</TableCell>
      <TableCell className="text-xs">{account.recidivism ?? 0}</TableCell>
      <TableCell className="text-xs text-muted-foreground">{t("antidegrade.until", { time: formatDateTime(account.quarantineUntil, locale) })}</TableCell>
      <TableCell className="text-right">
        <div className="flex flex-wrap justify-end gap-1">
          <Button size="sm" variant="ghost" className="h-7 px-2" disabled={pending} onClick={onDetect}>{t("antidegrade.detectBotFlag")}</Button>
          <Button size="sm" variant="ghost" className="h-7 px-2" disabled={pending} onClick={onVideo}>{t("antidegrade.testVideo")}</Button>
          <Button size="sm" variant="ghost" className="h-7 px-2" disabled={pending} onClick={onDisable}>{t("common.disable")}</Button>
          <Button size="sm" variant="secondary" className="h-7 px-2" disabled={pending} onClick={onClear}>{t("antidegrade.clearAccount")}</Button>
        </div>
      </TableCell>
    </TableRow>
  );
}

function EventPanel({ status, locale }: { status: AntiDegradeStatusDTO; locale: string }) {
  const { t } = useTranslation();
  return (
    <section className="overflow-hidden rounded-lg bg-card">
      <div className="border-b px-4 py-4 sm:px-5">
        <h2 className="text-sm font-medium">{t("antidegrade.events")}</h2>
      </div>
      <div className="max-h-72 overflow-auto">
        {status.events.length === 0 ? (
          <p className="px-4 py-10 text-center text-xs text-muted-foreground sm:px-5">{t("antidegrade.noEvents")}</p>
        ) : status.events.map((event, index) => (
          <div key={`${event.at}-${event.exitIp}-${index}`} className="grid grid-cols-[7.5rem_minmax(0,1fr)_auto] gap-3 border-b px-4 py-2 last:border-b-0 sm:px-5">
            <div className="font-mono text-[11px] text-muted-foreground">{formatDateTime(event.at, locale)}</div>
            <div className="truncate text-xs">{event.accountName || event.accountId || "—"} · {event.exitIp}</div>
            <div className="text-xs text-muted-foreground">{event.success ? t("antidegrade.eventOk") : t("antidegrade.eventMiss")}</div>
          </div>
        ))}
      </div>
    </section>
  );
}

function SettingsForm({
  form, setForm, saving, onSave, onCancel,
}: {
  form: AntiDegradeConfigDTO;
  setForm: (value: AntiDegradeConfigDTO) => void;
  saving: boolean;
  onSave: () => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  return (
    <form className="space-y-5" onSubmit={(event) => { event.preventDefault(); onSave(); }}>
      <div className="flex items-center gap-3">
        <Switch checked={form.enabled} onCheckedChange={(checked) => setForm({ ...form, enabled: checked })} />
        <Label>{t("antidegrade.enabled")}</Label>
      </div>
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <Field label={t("antidegrade.mode")}>
          <select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={form.mode} onChange={(event) => setForm({ ...form, mode: event.target.value as AntiDegradeConfigDTO["mode"] })}>
            <option value="enforce">{t("antidegrade.modes.enforce")}</option>
            <option value="observe">{t("antidegrade.modes.observe")}</option>
          </select>
        </Field>
        <Field label={t("antidegrade.densityWindow")}><Input value={form.densityWindow} onChange={(event) => setForm({ ...form, densityWindow: event.target.value })} /></Field>
        <Field label={t("antidegrade.densityMaxAccounts")}><Input type="number" min={1} max={50} value={form.densityMaxAccounts} onChange={(event) => setForm({ ...form, densityMaxAccounts: Number(event.target.value) })} /></Field>
        <Field label={t("antidegrade.failExitThreshold")} hint={t("antidegrade.failExitHelp")}>
          <Input type="number" min={1} max={10} value={form.failExitThreshold} onChange={(event) => setForm({ ...form, failExitThreshold: Number(event.target.value) })} />
        </Field>
        <Field label={t("antidegrade.maxIpRetries")}><Input type="number" min={1} max={6} value={form.maxIpRetries} onChange={(event) => setForm({ ...form, maxIpRetries: Number(event.target.value) })} /></Field>
        <Field label={t("antidegrade.thinkingMinOutput")}><Input type="number" min={8} max={256} value={form.thinkingMinOutput} onChange={(event) => setForm({ ...form, thinkingMinOutput: Number(event.target.value) })} /></Field>
        <Field label={t("antidegrade.dirtyIpCooldown")}><Input value={form.dirtyIpCooldown} onChange={(event) => setForm({ ...form, dirtyIpCooldown: event.target.value })} /></Field>
        <Field label={t("antidegrade.farmIpCooldown")}><Input value={form.farmIpCooldown} onChange={(event) => setForm({ ...form, farmIpCooldown: event.target.value })} /></Field>
        <Field label={t("antidegrade.accountQuarantineTtl")}><Input value={form.accountQuarantineTtl} onChange={(event) => setForm({ ...form, accountQuarantineTtl: event.target.value })} /></Field>
        <Field label={t("antidegrade.operatorOverride")}><Input value={form.operatorOverride} onChange={(event) => setForm({ ...form, operatorOverride: event.target.value })} /></Field>
      </div>
      <details>
        <summary className="cursor-pointer text-sm font-medium">{t("antidegrade.advanced")}</summary>
        <div className="mt-3 grid gap-4 sm:grid-cols-2">
          <Field label={t("antidegrade.scorePrior")}><Input type="number" step="0.05" min={0.05} max={1} value={form.scorePrior} onChange={(event) => setForm({ ...form, scorePrior: Number(event.target.value) })} /></Field>
          <Field label={t("antidegrade.exploreRatio")}><Input type="number" step="0.01" min={0} max={0.5} value={form.exploreRatio} onChange={(event) => setForm({ ...form, exploreRatio: Number(event.target.value) })} /></Field>
        </div>
      </details>
      <DialogFooter>
        <Button type="button" variant="secondary" onClick={onCancel}>{t("common.cancel")}</Button>
        <Button type="submit" disabled={saving}>{t("antidegrade.save")}</Button>
      </DialogFooter>
    </form>
  );
}

function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <div className="space-y-1.5">
      <Label>{label}</Label>
      {children}
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}
