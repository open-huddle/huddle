import { createConnectTransport } from "@connectrpc/connect-web";
import { createClient } from "@connectrpc/connect";
import { HealthService } from "@open-huddle/gen-ts/huddle/v1/health_pb";

export const transport = createConnectTransport({
  baseUrl: "", // same-origin; Vite proxies /huddle.v1 to the API in dev
});

export const healthClient = createClient(HealthService, transport);
