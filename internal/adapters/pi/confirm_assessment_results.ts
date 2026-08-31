import { Type } from "@sinclair/typebox";

export default function (pi) {
  pi.registerTool({
    name: "confirm_assessment_results",
    label: "Confirm assessment results",
    description: "Submit the complete assessment result batch to BOSS Job Agent. Ordinary text does not save results.",
    parameters: Type.Object({
      // Go validates every item independently so one malformed peer cannot
      // prevent valid results in the same tool call from reaching Confirm.
      results: Type.Array(Type.Unknown(), { minItems: 1, maxItems: 5 }),
    }),
    async execute(_toolCallId, params, signal) {
      const response = await fetch(process.env.BOSS_JOB_AGENT_CONFIRM_URL, {
        method: "POST",
        headers: {
          "Authorization": `Bearer ${process.env.BOSS_JOB_AGENT_CONFIRM_TOKEN}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify(params),
        signal,
      });
      const text = await response.text();
      if (!response.ok) {
        throw new Error(`confirmation callback failed with HTTP ${response.status}`);
      }
      return {
        content: [{ type: "text", text: "BOSS Job Agent 已逐项处理鉴定结果。" }],
        details: JSON.parse(text),
      };
    },
  });
}
