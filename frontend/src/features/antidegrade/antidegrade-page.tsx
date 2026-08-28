import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Activity, Network, Pencil, RefreshCw, ShieldCheck, ShieldX, TimerReset } from "lucide-react";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { toast } from "sonner";

import { ApiError } from "@/shared/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { clearAntiDegradeAccount, clearAntiDegradeIP, getAntiDegradeStatus, updateAntiDegradeConfig, type AntiDegradeConfigDTO, type AntiDegradeIPDTO, type AntiDegradeStatusDTO } from "@/features/antidegrade/antidegrade-api";
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
  const clearAccount = useMutation({
    mutationFn: clearAntiDegradeAccount,
    onSuccess: (status) => { applyStatus(status); toast.success(t("antidegrade.clearedAccount")); },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("common.retry")),
  });
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
            <QuarantinePanel status={status} locale={i18n.language} clearing={clearAccount.isPending} onClear={(id) => clearAccount.mutate(id)} />
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

function IPLoadPanel({ ips, locale, onClear, clearing }: { ips: AntiDegradeIPDTO[]; locale: string; onClear: (exitIp: string) => void; clearing: boolean }) {
  const { t } = useTranslation();
  return (
    <section className="overflow-hidden rounded-lg bg-card">
      <div className="border-b px-4 py-4 sm:px-5">
        <h2 className="text-sm font-medium">{t("antidegrade.loadTab")}</h2>
        <p className="mt-1 text-xs text-muted-foreground">{t("antidegrade.loadHelp")}</p>
      </div>
      {ips.length === 0 ? (
        <p className="px-4 py-10 text-center text-xs text-muted-foreground sm:px-5">{t("antidegrade.noIps")}</p>
      ) : ips.map((ip) => (
        <IPLoadRow key={ip.exitIp} ip={ip} locale={locale} onClear={() => onClear(ip.exitIp)} clearing={clearing} />
      ))}
    </section>
  );
}

function IPLoadRow({ ip, locale, onClear, clearing }: { ip: AntiDegradeIPDTO; locale: string; onClear: () => void; clearing: boolean }) {
  const { t } = useTranslation();
  const limit = Math.max(ip.accountLimit, 1);
  const ratio = Math.min(ip.accountCount / limit, 1);
  const tone = ip.cooling ? "bg-muted-foreground/40" : ratio >= 1 ? "bg-destructive" : ratio >= 0.6 ? "bg-amber-500" : "bg-emerald-500";
  const badge = ip.operatorOverrideUntil ? t("antidegrade.override") : ip.cooling ? t("antidegrade.cooling") : ratio >= 1 ? t("antidegrade.full") : ratio >= 0.6 ? t("antidegrade.nearFull") : t("antidegrade.idle");
  const reason = ip.cooldownReason ? t(`antidegrade.reasons.${ip.cooldownReason}`, { defaultValue: ip.cooldownReason }) : "";
  const title = (ip.exitIp.startsWith("node:") || ip.exitIp.startsWith("account:")) && ip.nodeNames[0] ? ip.nodeNames[0] : ip.exitIp;
  const names = ip.nodeNames.join(" · ") || (ip.nodeIds.length ? ip.nodeIds.map((id) => `#${id}`).join(" · ") : t("antidegrade.nodes"));
  return (
    <div className="border-b px-4 py-3 last:border-b-0 sm:px-5">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="truncate font-mono text-xs">{title}</p>
          <p className="mt-0.5 truncate text-[11px] text-muted-foreground">{names}</p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Badge variant="outline" className={cn("whitespace-nowrap", ip.cooling ? "text-muted-foreground" : ratio >= 1 ? "text-destructive" : ratio >= 0.6 ? "text-amber-700 dark:text-amber-400" : "text-emerald-600 dark:text-emerald-400")}>{badge}</Badge>
          {ip.cooling ? <Button size="sm" variant="secondary" disabled={clearing} onClick={onClear}>{t("antidegrade.clearIp")}</Button> : null}
        </div>
      </div>
      <div className="mt-2 grid grid-cols-[minmax(0,1fr)_auto] items-center gap-3">
        <div className="h-1.5 overflow-hidden rounded-full bg-muted">
          <i className={cn("block h-full", tone)} style={{ width: `${Math.max(ratio * 100, ip.accountCount > 0 ? 8 : 0)}%` }} />
        </div>
        <span className="font-mono text-xs tabular-nums text-muted-foreground">{ip.accountCount}/{ip.accountLimit}</span>
      </div>
      {ip.accounts.length > 0 ? (
        <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1">
          {ip.accounts.map((account) => (
            <Link key={account.id} to="/accounts" className="text-xs hover:underline">{account.name || account.id}</Link>
          ))}
        </div>
      ) : null}
      {ip.cooling && ip.cooldownUntil ? (
        <p className="mt-1 text-[11px] text-muted-foreground">{reason} · {t("antidegrade.until", { time: formatDateTime(ip.cooldownUntil, locale) })}</p>
      ) : null}
    </div>
  );
}

function QuarantinePanel({ status, locale, clearing, onClear }: { status: AntiDegradeStatusDTO; locale: string; clearing: boolean; onClear: (id: string) => void }) {
  const { t } = useTranslation();
  return (
    <section className="overflow-hidden rounded-lg bg-card">
      <div className="border-b px-4 py-4 sm:px-5">
        <h2 className="text-sm font-medium">{t("antidegrade.quarantined")}</h2>
        <p className="mt-1 text-xs text-muted-foreground">{t("antidegrade.quarantineHelp")}</p>
      </div>
      {status.quarantined.length === 0 ? (
        <p className="px-4 py-10 text-center text-xs text-muted-foreground sm:px-5">{t("antidegrade.noQuarantine")}</p>
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("antidegrade.accounts")}</TableHead>
                <TableHead>{t("antidegrade.failedIps")}</TableHead>
                <TableHead>{t("antidegrade.recidivism")}</TableHead>
                <TableHead>{t("antidegrade.cooldown")}</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {status.quarantined.map((account) => (
                <TableRow key={account.id}>
                  <TableCell>
                    <Link className="text-sm hover:underline" to="/accounts">{account.name || account.id}</Link>
                  </TableCell>
                  <TableCell className="font-mono text-xs">{account.failedExitIps.join(", ") || "—"}</TableCell>
                  <TableCell className="text-xs">{account.recidivism ?? 0}</TableCell>
                  <TableCell className="text-xs text-muted-foreground">{t("antidegrade.until", { time: formatDateTime(account.quarantineUntil, locale) })}</TableCell>
                  <TableCell className="text-right">
                    <Button size="sm" variant="secondary" disabled={clearing} onClick={() => onClear(account.id)}>
                      {t("antidegrade.clearAccount")}
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </section>
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
