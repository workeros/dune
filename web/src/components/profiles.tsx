import { useEffect, useRef, useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Input, Textarea } from "@/components/ui/input";
import {
  Dialog,
  DialogContent,
  DialogTitle,
  DialogDescription,
} from "@/components/ui/dialog";
import {
  errorText,
  request,
  type Profile,
  type ProfileCommand,
  type ProfileRecord,
  type Page,
} from "@/lib/api";

const blankProfile = (): Profile => ({
  version: 1,
  kind: "agent",
  working_directory: "/",
  env: {},
  setup: { steps: [] },
  start: { argv: [""] },
  adapter: "pty",
  history_lines: 50000,
});

type CommandDraft = {
  name: string;
  mode: "argv" | "shell";
  program: string;
  args: string;
  run: string;
  shell: string;
  timeout: number;
};
function commandDraft(command: ProfileCommand): CommandDraft {
  return {
    name: command.name ?? "",
    mode: command.run ? "shell" : "argv",
    program: command.argv?.[0] ?? "",
    args: JSON.stringify(command.argv?.slice(1) ?? []),
    run: command.run ?? "",
    shell: command.shell ?? "/bin/sh",
    timeout: command.timeout_seconds ?? 0,
  };
}
function commandValue(draft: CommandDraft): ProfileCommand {
  const common = { name: draft.name, timeout_seconds: draft.timeout };
  if (draft.mode === "shell")
    return { ...common, run: draft.run, shell: draft.shell };
  const args: unknown = JSON.parse(draft.args);
  if (!Array.isArray(args) || args.some((arg) => typeof arg !== "string"))
    throw new Error("命令参数必须是字符串 JSON 数组。");
  return { ...common, argv: [draft.program, ...args] };
}
function CommandEditor({
  value,
  onChange,
  label,
}: {
  value: CommandDraft;
  onChange: (value: CommandDraft) => void;
  label: string;
}) {
  return (
    <fieldset className="grid gap-3 rounded-lg border border-foreground/20 p-3">
      <legend className="px-1 font-semibold">{label}</legend>
      <label>
        步骤名称
        <Input
          value={value.name}
          onChange={(e) => onChange({ ...value, name: e.target.value })}
        />
      </label>
      <label>
        命令方式
        <select
          value={value.mode}
          onChange={(e) =>
            onChange({ ...value, mode: e.target.value as CommandDraft["mode"] })
          }
        >
          <option value="argv">程序与参数</option>
          <option value="shell">Shell 脚本</option>
        </select>
      </label>
      {value.mode === "argv" ? (
        <>
          <label>
            程序
            <Input
              value={value.program}
              required
              placeholder="例如 codex 或 /absolute/path/to/agent"
              onChange={(e) => onChange({ ...value, program: e.target.value })}
            />
          </label>
          <label>
            参数（JSON 数组）
            <Textarea
              value={value.args}
              onChange={(e) => onChange({ ...value, args: e.target.value })}
              className="font-mono text-xs"
              spellCheck={false}
            />
          </label>
        </>
      ) : (
        <>
          <label>
            Shell 路径
            <Input
              value={value.shell}
              required
              onChange={(e) => onChange({ ...value, shell: e.target.value })}
            />
          </label>
          <label>
            脚本
            <Textarea
              value={value.run}
              required
              onChange={(e) => onChange({ ...value, run: e.target.value })}
              className="font-mono text-xs"
              spellCheck={false}
            />
          </label>
        </>
      )}
      <label>
        超时秒数（0 使用默认值）
        <Input
          type="number"
          min={0}
          max={86400}
          value={value.timeout}
          onChange={(e) =>
            onChange({ ...value, timeout: Number(e.target.value) })
          }
        />
      </label>
    </fieldset>
  );
}

export function ProfileManager({
  open,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const opener = useRef<HTMLElement | null>(null);
  const [records, setRecords] = useState<ProfileRecord[]>([]),
    [selected, setSelected] = useState<ProfileRecord>();
  const [name, setName] = useState(""),
    [description, setDescription] = useState(""),
    [profile, setProfile] = useState(blankProfile);
  const [env, setEnv] = useState("{}"),
    [steps, setSteps] = useState<CommandDraft[]>([]),
    [start, setStart] = useState(() => commandDraft({}));
  const [error, setError] = useState(""),
    [busy, setBusy] = useState(false),
    [loading, setLoading] = useState(false),
    [confirmDelete, setConfirmDelete] = useState(false);
  const choose = (record?: ProfileRecord) => {
    const next = record?.profile ?? blankProfile();
    setSelected(record);
    setName(record?.name ?? "");
    setDescription(record?.description ?? "");
    setProfile(next);
    setEnv(JSON.stringify(next.env ?? {}, null, 2));
    setSteps((next.setup.steps ?? []).map(commandDraft));
    setStart(commandDraft(next.start));
    setError("");
    setConfirmDelete(false);
  };
  useEffect(() => {
    if (!open) return;
    let current = true;
    choose();
    setRecords([]);
    setLoading(true);
    request<Page<ProfileRecord>>("/api/v1/profiles")
      .then((page) => {
        if (current) setRecords(page.items);
      })
      .catch((e) => {
        if (current) setError(errorText(e));
      })
      .finally(() => {
        if (current) setLoading(false);
      });
    return () => {
      current = false;
    };
  }, [open]);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const variables: unknown = JSON.parse(env);
      if (
        !variables ||
        typeof variables !== "object" ||
        Array.isArray(variables) ||
        Object.values(variables).some((value) => typeof value !== "string")
      )
        throw new Error("环境变量必须是字符串值的 JSON 对象。");
      const complete: Profile = {
        ...profile,
        env: variables as Record<string, string>,
        setup: { steps: steps.map(commandValue) },
        start: profile.kind === "agent" ? commandValue(start) : {},
        adapter: profile.kind === "agent" ? profile.adapter : "",
        history_lines:
          profile.kind === "agent" && profile.adapter === "pty"
            ? profile.history_lines
            : 0,
        managed_acp: false,
      };
      await request<ProfileRecord>(
        `/api/v1/profiles${selected ? `/${encodeURIComponent(selected.id)}` : ""}`,
        {
          method: selected ? "PUT" : "POST",
          body: JSON.stringify({
            name,
            description,
            revision: selected?.revision ?? 0,
            profile: complete,
          }),
        },
      );
      onSaved();
      onOpenChange(false);
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    if (!selected) return;
    setBusy(true);
    setError("");
    try {
      await request(
        `/api/v1/profiles/${encodeURIComponent(selected.id)}?revision=${selected.revision}`,
        { method: "DELETE" },
      );
      setRecords((items) => items.filter((item) => item.id !== selected.id));
      choose();
      onSaved();
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!busy) onOpenChange(next);
      }}
    >
      <DialogContent
        onOpenAutoFocus={() => {
          opener.current =
            document.activeElement instanceof HTMLElement
              ? document.activeElement
              : null;
        }}
        onCloseAutoFocus={(event) => {
          event.preventDefault();
          opener.current?.focus();
        }}
      >
        <DialogTitle className="mb-2 text-xl font-bold">
          Profile 管理
        </DialogTitle>
        <DialogDescription className="mb-5 text-sm text-muted-foreground">
          配置保存在服务端，可在不同开发环境中复用。保存无需连接开发机，每次编辑生成新版本。
        </DialogDescription>
        {loading ? (
          <p role="status">正在加载配置…</p>
        ) : (
          <form className="grid gap-4" onSubmit={submit}>
            <fieldset className="grid gap-4" disabled={busy}>
              <label>
                配置
                <select
                  value={selected?.id ?? ""}
                  onChange={(e) =>
                    choose(
                      records.find((record) => record.id === e.target.value),
                    )
                  }
                >
                  <option value="">新建 Profile</option>
                  {records.map((record) => (
                    <option key={record.id} value={record.id}>
                      {record.name} · v{record.revision} ·{" "}
                      {record.profile.kind === "agent" ? "Agent" : "环境"}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                名称
                <Input
                  value={name}
                  required
                  maxLength={120}
                  onChange={(e) => setName(e.target.value)}
                />
              </label>
              <label>
                描述
                <Textarea
                  value={description}
                  maxLength={1024}
                  onChange={(e) => setDescription(e.target.value)}
                />
              </label>
              <label>
                配置类型
                <select
                  disabled={!!selected}
                  value={profile.kind}
                  onChange={(e) =>
                    setProfile({
                      ...profile,
                      kind: e.target.value as Profile["kind"],
                      adapter: "pty",
                    })
                  }
                >
                  <option value="agent">Agent：准备并启动</option>
                  <option value="environment">环境：仅准备</option>
                </select>
              </label>
              <label>
                默认工作目录
                <Input
                  value={profile.working_directory}
                  required
                  onChange={(e) =>
                    setProfile({
                      ...profile,
                      working_directory: e.target.value,
                    })
                  }
                />
              </label>
              <label>
                环境变量（JSON 对象）
                <Textarea
                  value={env}
                  onChange={(e) => setEnv(e.target.value)}
                  className="font-mono text-xs"
                  spellCheck={false}
                />
              </label>
              <div className="grid gap-3">
                <p className="font-semibold">准备步骤</p>
                {steps.map((step, index) => (
                  <div key={index} className="grid gap-2">
                    <CommandEditor
                      label={`步骤 ${index + 1}`}
                      value={step}
                      onChange={(value) =>
                        setSteps((items) =>
                          items.map((item, i) => (i === index ? value : item)),
                        )
                      }
                    />
                    <div className="flex gap-2">
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        disabled={index === 0}
                        onClick={() =>
                          setSteps((items) => {
                            const next = [...items];
                            [next[index - 1], next[index]] = [
                              next[index],
                              next[index - 1],
                            ];
                            return next;
                          })
                        }
                      >
                        上移步骤
                      </Button>
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={() =>
                          setSteps((items) =>
                            items.filter((_, i) => i !== index),
                          )
                        }
                      >
                        删除步骤
                      </Button>
                    </div>
                  </div>
                ))}
                <Button
                  type="button"
                  variant="outline"
                  disabled={steps.length >= 64}
                  onClick={() =>
                    setSteps((items) => [...items, commandDraft({})])
                  }
                >
                  添加准备步骤
                </Button>
              </div>
              {profile.kind === "agent" && (
                <>
                  <CommandEditor
                    label="启动命令"
                    value={start}
                    onChange={setStart}
                  />
                  <label>
                    交互方式
                    <select
                      value={profile.adapter}
                      onChange={(e) =>
                        setProfile({
                          ...profile,
                          adapter: e.target.value as "pty" | "acp",
                        })
                      }
                    >
                      <option value="pty">PTY</option>
                      <option value="acp">ACP</option>
                    </select>
                  </label>
                  {profile.adapter === "pty" && (
                    <label>
                      保留历史行数
                      <Input
                        type="number"
                        min={0}
                        max={200000}
                        value={profile.history_lines ?? 50000}
                        onChange={(e) =>
                          setProfile({
                            ...profile,
                            history_lines: Number(e.target.value),
                          })
                        }
                      />
                    </label>
                  )}
                </>
              )}
              <div className="flex flex-wrap justify-between gap-3">
                {selected && (
                  <Button
                    type="button"
                    variant="ghost"
                    onClick={() => setConfirmDelete(true)}
                  >
                    删除 Profile
                  </Button>
                )}
                <Button type="submit" className="ml-auto">
                  {busy
                    ? "保存中…"
                    : selected
                      ? `保存为 v${selected.revision + 1}`
                      : "保存 Profile"}
                </Button>
              </div>
              {confirmDelete && (
                <div className="rounded border border-destructive p-3">
                  <p className="mb-2 text-sm">
                    确认删除“{selected?.name}”及其保存的版本？
                  </p>
                  <Button
                    type="button"
                    variant="destructive"
                    onClick={() => void remove()}
                  >
                    确认删除
                  </Button>
                </div>
              )}
            </fieldset>
          </form>
        )}
        {error && (
          <div role="alert" className="error-box mt-4">
            {error}
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
