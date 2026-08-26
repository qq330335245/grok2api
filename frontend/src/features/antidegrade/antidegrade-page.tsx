import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { RefreshCw } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { clearAntiDegradeAccount, clearAntiDegradeIP, getAntiDegradeStatus, updateAntiDegradeConfig, type AntiDegradeConfigDTO, type AntiDegradeIPDTO, type AntiDegradeStatusDTO } from "@/features/antidegrade/antidegrade-api";
import { EmptyState, ErrorState, LoadingState } from "@/shared/components/data-state";
import { PageHeader } from "@/shared/components/page-header";
import { ApiError } from "@/shared/api/client";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";

export function AntidegradePage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const statusQuery = useQuery({
    queryKey: ["antidegrade"],
    queryFn: getAntiDegradeStatus,
    refetchInterval: 5_000,
  });
  const [form, setForm] = useState<AntiDegradeConfigDTO | null>(null);
  useEffect(() => {
    if (statusQuery.data && !form) setForm(statusQuery.data.config);
  }, [statusQuery.data, form]);

  const applyStatus = (status: AntiDegradeStatusDTO) => {
    queryClient.setQueryData(["antidegrade"], status);
    setForm(status.config);
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
    mutationFn: () => {
      if (!form || !statusQuery.data) throw new Error("missing form");
      return updateAntiDegradeConfig(statusQuery.data.revision, form);
    },
    onSuccess: (status) => { applyStatus(status); toast.success(t("antidegrade.saved")); },
    onError: (error) => {
      if (error instanceof ApiError && error.status === 409) {
        toast.error(t("antidegrade.conflict"));
        void statusQuery.refetch().then((result) => { if (result.data) setForm(result.data.config); });
        return;
      }
      toast.error(error instanceof Error ? error.message : t("common.retry"));
    },
  });

  const status = statusQuery.data;
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
          <p className="text-sm text-muted-foreground">
            {t("antidegrade.summary", {
              mode: t(`antidegrade.modes.${status.config.mode}`),
              window: status.config.densityWindow,
              cap: status.config.densityMaxAccounts,
              threshold: status.config.failExitThreshold,
            })}
          </p>
          <Tabs defaultValue="load">
            <TabsList>
              <TabsTrigger value="load">{t("antidegrade.loadTab")}</TabsTrigger>
              <TabsTrigger value="quarantine">{t("antidegrade.quarantineTab")}</TabsTrigger>
              <TabsTrigger value="settings">{t("antidegrade.settingsTab")}</TabsTrigger>
            </TabsList>
            <TabsContent value="load" className="mt-6 space-y-6">
              <p className="text-xs text-muted-foreground">{t("antidegrade.loadHelp")}</p>
              {status.ips.length === 0 ? <EmptyState message={t("antidegrade.noIps")} /> : (
                <div className="space-y-3">
                  {status.ips.map((ip) => <IPLoadCard key={ip.exitIp} ip={ip} locale={i18n.language} onClear={() => clearIP.mutate(ip.exitIp)} clearing={clearIP.isPending} />)}
                </div>
              )}
              <EventTable status={status} locale={i18n.language} />
            </TabsContent>
            <TabsContent value="quarantine" className="mt-6">
              {status.quarantined.length === 0 ? <EmptyState message={t("antidegrade.noQuarantine")} /> : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t("antidegrade.accounts")}</TableHead>
                      <TableHead>{t("antidegrade.failedIps")}</TableHead>
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
                        <TableCell className="text-sm">{t("antidegrade.until", { time: formatDateTime(account.quarantineUntil, i18n.language) })}</TableCell>
                        <TableCell className="text-right">
                          <Button size="sm" variant="secondary" disabled={clearAccount.isPending} onClick={() => clearAccount.mutate(account.id)}>
                            {t("antidegrade.clearAccount")}
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </TabsContent>
            <TabsContent value="settings" className="mt-6">
              {form ? (
                <form className="max-w-3xl space-y-5" onSubmit={(event) => { event.preventDefault(); saveConfig.mutate(); }}>
                  <p className="text-xs text-muted-foreground">{t("antidegrade.saveHint")}</p>
                  <div className="flex items-center gap-3">
                    <Switch checked={form.enabled} onCheckedChange={(checked) => setForm({ ...form, enabled: checked })} />
                    <Label>{t("antidegrade.enabled")}</Label>
                  </div>
                  <div className="grid gap-4 sm:grid-cols-2">
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
                  <details className="rounded-md border p-3">
                    <summary className="cursor-pointer text-sm font-medium">{t("antidegrade.advanced")}</summary>
                    <div className="mt-3 grid gap-4 sm:grid-cols-2">
                      <Field label={t("antidegrade.scorePrior")}><Input type="number" step="0.05" min={0.05} max={1} value={form.scorePrior} onChange={(event) => setForm({ ...form, scorePrior: Number(event.target.value) })} /></Field>
                      <Field label={t("antidegrade.exploreRatio")}><Input type="number" step="0.01" min={0} max={0.5} value={form.exploreRatio} onChange={(event) => setForm({ ...form, exploreRatio: Number(event.target.value) })} /></Field>
                    </div>
                  </details>
                  <Button type="submit" disabled={saveConfig.isPending}>{t("antidegrade.save")}</Button>
                </form>
              ) : null}
            </TabsContent>
          </Tabs>
        </>
      ) : null}
    </div>
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

function IPLoadCard({ ip, locale, onClear, clearing }: { ip: AntiDegradeIPDTO; locale: string; onClear: () => void; clearing: boolean }) {
  const { t } = useTranslation();
  const limit = Math.max(ip.accountLimit, 1);
  const ratio = Math.min(ip.accountCount / limit, 1);
  const tone = ip.cooling ? "bg-muted-foreground/40" : ratio >= 1 ? "bg-red-500" : ratio >= 0.6 ? "bg-amber-500" : "bg-emerald-500";
  const badge = ip.operatorOverrideUntil ? t("antidegrade.override") : ip.cooling ? t("antidegrade.cooling") : ratio >= 1 ? t("antidegrade.full") : ratio >= 0.6 ? t("antidegrade.nearFull") : t("antidegrade.idle");
  const reason = ip.cooldownReason ? t(`antidegrade.reasons.${ip.cooldownReason}`, { defaultValue: ip.cooldownReason }) : "";
  return (
    <div className="rounded-lg border p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <p className="font-mono text-sm">{ip.exitIp}</p>
          <p className="text-xs text-muted-foreground">{ip.nodeNames.join(" · ") || t("antidegrade.nodes")}</p>
        </div>
        <div className="flex items-center gap-2">
          <Badge variant="secondary">{badge}</Badge>
          {ip.cooling ? (
            <Button size="sm" variant="secondary" disabled={clearing} onClick={onClear}>{t("antidegrade.clearIp")}</Button>
          ) : null}
        </div>
      </div>
      <div className="mt-3 flex items-center gap-3">
        <div className="h-2 flex-1 overflow-hidden rounded-full bg-muted">
          <div className={cn("h-full rounded-full", tone)} style={{ width: `${Math.max(ratio * 100, ip.accountCount > 0 ? 8 : 0)}%` }} />
        </div>
        <span className="w-12 text-right text-sm tabular-nums">{ip.accountCount}/{ip.accountLimit}</span>
      </div>
      <div className="mt-2 flex flex-wrap gap-1.5">
        {ip.accounts.map((account) => (
          <Link key={account.id} to="/accounts" className="rounded-full bg-muted px-2 py-0.5 text-xs hover:underline">{account.name || account.id}</Link>
        ))}
      </div>
      {ip.cooling && ip.cooldownUntil ? (
        <p className="mt-2 text-xs text-muted-foreground">{reason} · {t("antidegrade.until", { time: formatDateTime(ip.cooldownUntil, locale) })}</p>
      ) : null}
    </div>
  );
}

function EventTable({ status, locale }: { status: AntiDegradeStatusDTO; locale: string }) {
  const { t } = useTranslation();
  return (
    <section>
      <h2 className="mb-3 text-sm font-medium">{t("antidegrade.events")}</h2>
      {status.events.length === 0 ? <EmptyState message={t("antidegrade.noEvents")} /> : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("antidegrade.accounts")}</TableHead>
              <TableHead>{t("antidegrade.exitIp")}</TableHead>
              <TableHead />
              <TableHead>{t("antidegrade.cooldown")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {status.events.map((event, index) => (
              <TableRow key={`${event.at}-${event.exitIp}-${index}`}>
                <TableCell>{event.accountName || event.accountId || "—"}</TableCell>
                <TableCell className="font-mono text-xs">{event.exitIp}</TableCell>
                <TableCell>{event.success ? t("antidegrade.eventOk") : t("antidegrade.eventMiss")}</TableCell>
                <TableCell className="text-xs text-muted-foreground">{formatDateTime(event.at, locale)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}
