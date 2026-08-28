import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";

export type AntiDegradeConfigDTO = {
  enabled: boolean;
  mode: "observe" | "enforce";
  providers: string[];
  thinkingMinOutput: number;
  densityWindow: string;
  densityMaxAccounts: number;
  dirtyIpCooldown: string;
  farmIpCooldown: string;
  maxIpRetries: number;
  failExitThreshold: number;
  accountQuarantineTtl: string;
  scorePrior: number;
  exploreRatio: number;
  operatorOverride: string;
};

export type AntiDegradeAccountRefDTO = {
  id: string;
  name?: string;
};

export type AntiDegradeIPDTO = {
  exitIp: string;
  nodeIds: string[];
  nodeNames: string[];
  accounts: AntiDegradeAccountRefDTO[];
  accountCount: number;
  accountLimit: number;
  cooling: boolean;
  cooldownUntil?: string;
  cooldownReason?: string;
  operatorOverrideUntil?: string;
  score: number;
};

export type AntiDegradeQuarantineDTO = {
  id: string;
  name?: string;
  failedExitIps: string[];
  quarantineUntil: string;
  quarantineReason?: string;
  consecutive?: number;
  recidivism?: number;
};

export type AntiDegradeEventDTO = {
  at: string;
  success: boolean;
  accountId?: string;
  accountName?: string;
  exitIp: string;
};

export type AntiDegradeStatusDTO = {
  revision: string;
  config: AntiDegradeConfigDTO;
  ips: AntiDegradeIPDTO[];
  quarantined: AntiDegradeQuarantineDTO[];
  events: AntiDegradeEventDTO[];
};

const configValidator = hasShape({
  enabled: isBoolean,
  mode: isOneOf("observe", "enforce"),
  providers: isOptional(isArrayOf(isString)),
  thinkingMinOutput: isNumber,
  densityWindow: isString,
  densityMaxAccounts: isNumber,
  dirtyIpCooldown: isString,
  farmIpCooldown: isString,
  maxIpRetries: isNumber,
  failExitThreshold: isNumber,
  accountQuarantineTtl: isString,
  scorePrior: isNumber,
  exploreRatio: isNumber,
  operatorOverride: isString,
});

const decodeStatus = createObjectDecoder<AntiDegradeStatusDTO>("antidegrade status", {
  revision: isString,
  config: configValidator,
  ips: isArrayOf(hasShape({
    exitIp: isString, nodeIds: isArrayOf(isString), nodeNames: isArrayOf(isString),
    accounts: isArrayOf(hasShape({ id: isString, name: isOptional(isString) })),
    accountCount: isNumber, accountLimit: isNumber, cooling: isBoolean,
    cooldownUntil: isOptional(isString), cooldownReason: isOptional(isString),
    operatorOverrideUntil: isOptional(isString), score: isNumber,
  })),
  quarantined: isArrayOf(hasShape({
    id: isString, name: isOptional(isString), failedExitIps: isArrayOf(isString),
    quarantineUntil: isString, quarantineReason: isOptional(isString),
    consecutive: isOptional(isNumber), recidivism: isOptional(isNumber),
  })),
  events: isArrayOf(hasShape({
    at: isString, success: isBoolean, accountId: isOptional(isString),
    accountName: isOptional(isString), exitIp: isString,
  })),
});

export function getAntiDegradeStatus(): Promise<AntiDegradeStatusDTO> {
  return apiRequest("/api/admin/v1/antidegrade/status", {}, decodeStatus);
}

export function updateAntiDegradeConfig(revision: string, config: AntiDegradeConfigDTO): Promise<AntiDegradeStatusDTO> {
  return apiRequest("/api/admin/v1/antidegrade/config", { method: "PUT", body: { revision, config } }, decodeStatus);
}

export function clearAntiDegradeAccount(id: string): Promise<AntiDegradeStatusDTO> {
  return apiRequest(`/api/admin/v1/antidegrade/accounts/${id}/clear`, { method: "POST" }, decodeStatus);
}

export function clearAntiDegradeIP(exitIp: string): Promise<AntiDegradeStatusDTO> {
  return apiRequest("/api/admin/v1/antidegrade/ips/clear", { method: "POST", body: { exitIp } }, decodeStatus);
}
