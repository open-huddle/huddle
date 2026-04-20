import { useEffect, useState } from "react";
import { Card, Space, Tag, Typography } from "antd";
import { healthClient } from "./transport";
import { CheckResponse_Status } from "@open-huddle/gen-ts/huddle/v1/health_pb";

const { Title, Paragraph } = Typography;

type State =
  | { kind: "loading" }
  | { kind: "ok"; status: CheckResponse_Status; version: string }
  | { kind: "error"; message: string };

export default function App() {
  const [state, setState] = useState<State>({ kind: "loading" });

  useEffect(() => {
    healthClient
      .check({})
      .then((res) => setState({ kind: "ok", status: res.status, version: res.version }))
      .catch((err: unknown) =>
        setState({ kind: "error", message: err instanceof Error ? err.message : String(err) }),
      );
  }, []);

  return (
    <Space direction="vertical" style={{ padding: 32, width: "100%" }} size="large">
      <Title level={2}>Open Huddle</Title>
      <Card title="API health">
        {state.kind === "loading" && <Paragraph>Checking…</Paragraph>}
        {state.kind === "error" && <Tag color="red">error: {state.message}</Tag>}
        {state.kind === "ok" && (
          <Space>
            <Tag color={state.status === CheckResponse_Status.SERVING ? "green" : "orange"}>
              {CheckResponse_Status[state.status]}
            </Tag>
            <Paragraph style={{ margin: 0 }}>version: {state.version}</Paragraph>
          </Space>
        )}
      </Card>
    </Space>
  );
}
