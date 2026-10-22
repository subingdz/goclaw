import { describe, expect, it } from "vitest";
import type {
  AgentData,
  DeliveryBehaviorConfig,
  ModelRoutingConfig,
  ModelRoutingTier,
} from "@/types/agent";
import {
  buildAdvancedUpdatePayload,
  deriveState,
  normalizeModelRoutingForPayload,
} from "./agent-advanced-state-utils";

function buildPayload(
  deliveryBehaviorMode: "inherit" | "custom",
  deliveryBehavior: DeliveryBehaviorConfig,
  modelRoutingMode: "inherit" | "custom" = "inherit",
  modelRouting: ModelRoutingConfig = {},
) {
  return buildAdvancedUpdatePayload({
    agent: {
      id: "agent-1",
      agent_key: "support",
      provider: "openai",
      model: "gpt-4.1-mini",
      other_config: {
        keep_me: true,
        delivery_behavior: {
          quick_ack: { enabled: false },
        },
      },
    } as unknown as AgentData,
    currentProvider: undefined,
    providersLoading: false,
    providerModelsLoading: false,
    expertReasoningAvailable: false,
    reasoningMode: "inherit",
    reasoningEffort: "off",
    reasoningExpert: false,
    reasoningFallback: "downgrade",
    thinkingLevel: "off",
    chatgptRouting: {},
    modelFallback: { enabled: false, strategy: "priority_order", candidates: [] },
    wsSharing: {},
    comp: {},
    deliveryBehaviorMode,
    deliveryBehavior,
    inboundDebounceMode: "inherit",
    inboundDebounceMs: 0,
    modelRoutingMode,
    modelRouting,
    pruneEnabled: false,
    prune: {},
    sbEnabled: false,
    sb: {},
  });
}

describe("agent advanced delivery behavior payload", () => {
  it("saves custom delivery behavior without overwriting unrelated other_config", () => {
    const deliveryBehavior: DeliveryBehaviorConfig = {
      enabled: true,
      intermediate_replies: { enabled: true, mode: "sidecar_generated", provider: "groq", model: "llama-progress", timeout_ms: 5000 },
      quick_ack: { enabled: true, mode: "sidecar_generated", provider: "groq", model: "llama-ack", timeout_ms: 3500 },
    };

    const payload = buildPayload("custom", deliveryBehavior);

    expect(payload.other_config).toMatchObject({
      keep_me: true,
      delivery_behavior: deliveryBehavior,
    });
  });

  it("removes only delivery_behavior when inheriting", () => {
    const payload = buildPayload("inherit", {});

    expect(payload.other_config).toEqual({ keep_me: true });
  });
});

describe("agent advanced model routing payload", () => {
  const tiers: ModelRoutingTier[] = [
    { name: "heavy", model: "deepseek-r1", min_runes: 800, requires_code: true },
    { name: "cheap", model: "glm-4-flash", provider: "zhipu" },
  ];

  it("writes model_routing without overwriting unrelated other_config", () => {
    const payload = buildPayload("inherit", {}, "custom", { enabled: true, tiers });

    expect(payload.other_config).toMatchObject({
      keep_me: true,
      model_routing: { enabled: true, tiers },
    });
  });

  it("keeps routing and delivery behavior side by side", () => {
    // Both cards chain onto the same bag, and agents.update replaces the whole column — so one
    // save must not erase the other.
    const payload = buildPayload(
      "custom",
      { enabled: true, quick_ack: { enabled: true } },
      "custom",
      { enabled: true, tiers },
    );

    expect(payload.other_config).toMatchObject({
      delivery_behavior: { enabled: true },
      model_routing: { enabled: true },
    });
  });

  it("removes only model_routing when turning the card off", () => {
    const payload = buildPayload("custom", { enabled: true }, "inherit", { enabled: true, tiers });

    expect(payload.other_config).not.toHaveProperty("model_routing");
    expect(payload.other_config).toHaveProperty("delivery_behavior");
    expect(payload.other_config).toHaveProperty("keep_me");
  });

  it("drops tier rows the operator left half-filled", () => {
    const normalized = normalizeModelRoutingForPayload({
      enabled: true,
      tiers: [{ model: "  deepseek-r1  " }, { model: "   " }, { hint: "no model at all" }],
    });

    expect(normalized.tiers).toEqual([{ model: "deepseek-r1" }]);
  });

  it("refuses to save an enabled policy with nothing in it", () => {
    // internal/store.ValidateModelRouting rejects enabled+zero usable tiers, so the card must never
    // produce it: the save would fail with an error the operator cannot trace to this row.
    expect(normalizeModelRoutingForPayload({ enabled: true, tiers: [{ hint: "x" }] })).toEqual({
      enabled: false,
      tiers: [],
    });
  });

  it("drops a classifier that has no model", () => {
    expect(
      normalizeModelRoutingForPayload({
        enabled: true,
        tiers,
        classifier: { model: "  ", provider: "silicon", timeout_ms: 3000 },
      }),
    ).not.toHaveProperty("classifier");

    expect(
      normalizeModelRoutingForPayload({
        enabled: true,
        tiers,
        classifier: { model: "mini", timeout_ms: 0, cache_ttl_seconds: -5 },
      }).classifier,
    ).toEqual({ model: "mini" });
  });

  it("caps the tier list and preserves order", () => {
    const many = Array.from({ length: 12 }, (_, i) => ({ model: `m-${i}` }));
    const normalized = normalizeModelRoutingForPayload({ enabled: true, tiers: many });

    expect(normalized.tiers).toHaveLength(8);
    expect(normalized.tiers?.[0]?.model).toBe("m-0");
    expect(normalized.tiers?.[7]?.model).toBe("m-7");
  });

  it("strips zero thresholds and blank strings instead of storing them", () => {
    const normalized = normalizeModelRoutingForPayload({
      enabled: true,
      tiers: [
        {
          name: "  ",
          model: "m",
          provider: " ",
          hint: "",
          min_runes: 0,
          min_tokens: 0,
          requires_code: false,
          run_kinds: ["", " delegation "],
        },
      ],
    });

    expect(normalized.tiers?.[0]).toEqual({ model: "m", run_kinds: ["delegation"] });
  });

  it("re-reads a stored policy through the same normalizer", () => {
    const state = deriveState(
      {
        id: "agent-1",
        agent_key: "support",
        provider: "openai",
        model: "gpt-4.1-mini",
        other_config: {
          model_routing: {
            enabled: true,
            tiers: [{ name: "heavy", model: "deepseek-r1", min_runes: 800 }, { hint: "half row" }],
            classifier: { model: "" },
          },
        },
      } as unknown as AgentData,
      undefined,
    );

    expect(state.modelRoutingMode).toBe("custom");
    expect(state.modelRouting.tiers).toEqual([{ name: "heavy", model: "deepseek-r1", min_runes: 800 }]);
    expect(state.modelRouting.classifier).toBeUndefined();
  });

  it("shows an empty card when the agent has no policy", () => {
    const state = deriveState(
      {
        id: "agent-1",
        agent_key: "support",
        provider: "openai",
        model: "gpt-4.1-mini",
        other_config: { prompt_mode: "full" },
      } as unknown as AgentData,
      undefined,
    );

    expect(state.modelRoutingMode).toBe("inherit");
    expect(state.modelRouting.tiers).toEqual([]);
  });
});
