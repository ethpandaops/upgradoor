import { useId, useMemo, useState, type SyntheticEvent } from 'react';

import { errorText } from '../api/client';
import type { SettingsBody } from '../api/types';
import { useApp } from '../appContext';
import { Badge, Button, ErrorBanner, INPUT_CLASS, Loading, Table, Td, Th, cx } from '../components/primitives';
import { usePoll } from '../hooks/usePoll';
import {
  FIELDS,
  POLICIES,
  dirtyKeys,
  displayValue,
  fromForm,
  sameValue,
  toForm,
  validateForm,
  type FieldSpec,
  type SettingsForm,
} from '../lib/settingsForm';

/**
 * SettingsTab edits the scheduler settings. The form is loaded once (it is
 * not auto-refreshed, so typing is never overwritten) and saved with PUT.
 */
export function SettingsTab() {
  const { api } = useApp();
  const loaded = usePoll(s => api.settings(s), { key: 'settings-form', intervalMs: 0 });
  const [saved, setSaved] = useState<SettingsBody | null>(null);

  // The answer of the last save is newer than what was loaded.
  const body = saved ?? loaded.data;

  if (!body) {
    return loaded.loading ? (
      <Loading what="settings" />
    ) : (
      <ErrorBanner error={loaded.error} what="the settings" onRetry={loaded.refresh} />
    );
  }

  return (
    <SettingsEditor
      // A new stored state (after a save or a reload) resets the form.
      key={JSON.stringify(body.settings)}
      body={body}
      onSaved={setSaved}
      onReload={() => {
        setSaved(null);
        loaded.refresh();
      }}
    />
  );
}

function SettingsEditor({
  body,
  onSaved,
  onReload,
}: {
  body: SettingsBody;
  onSaved: (body: SettingsBody) => void;
  onReload: () => void;
}) {
  const { api } = useApp();
  const formId = useId();

  const [form, setForm] = useState<SettingsForm>(() => toForm(body.settings));
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(undefined);

  const errors = useMemo(() => validateForm(form), [form]);
  const dirty = useMemo(() => dirtyKeys(form, body.settings), [form, body.settings]);
  const invalid = Object.keys(errors).length > 0;

  const set = (key: FieldSpec['key'], value: string) => {
    setForm(prev => ({ ...prev, [key]: value }));
  };

  const submit = async (e: SyntheticEvent) => {
    e.preventDefault();
    if (invalid || dirty.length === 0 || saving) return;

    setSaving(true);
    setSaveError(undefined);
    try {
      onSaved(await api.putSettings(fromForm(form)));
    } catch (err) {
      setSaveError(err);
    } finally {
      setSaving(false);
    }
  };

  return (
    <form id={formId} aria-label="Settings" onSubmit={e => void submit(e)} className="flex flex-col gap-2" noValidate>
      <Table label="Settings">
        <thead>
          <tr>
            <Th>Setting</Th>
            <Th>Value</Th>
            <Th>Default</Th>
            <Th className="w-full">What it does</Th>
          </tr>
        </thead>
        <tbody>
          {FIELDS.map(spec => {
            const value = form[spec.key];
            const error = errors[spec.key];
            const isDirty = dirty.includes(spec.key);
            const defaultValue = displayValue(body.defaults, spec.key);
            const isDefault = sameValue(spec, value, defaultValue);
            const inputId = `${formId}-${spec.key}`;
            const hintId = `${inputId}-hint`;

            return (
              <tr key={spec.key} className={cx(isDirty && 'bg-surface-selected')}>
                <Td>
                  <label htmlFor={inputId} className="font-medium">
                    {spec.label}
                  </label>
                  <div className="font-mono text-faint">{spec.key}</div>
                </Td>
                <Td>
                  <span className="inline-flex items-center gap-1.5">
                    <FieldInput
                      id={inputId}
                      hintId={hintId}
                      spec={spec}
                      value={value}
                      invalid={error !== undefined}
                      onChange={v => {
                        set(spec.key, v);
                      }}
                    />
                    {isDirty && <Badge tone="info">changed</Badge>}
                  </span>
                  {error && (
                    <div role="alert" className="text-danger">
                      {error}
                    </div>
                  )}
                </Td>
                <Td>
                  <span className="inline-flex items-center gap-1.5">
                    <span className="font-mono">
                      {spec.kind === 'switch' ? (defaultValue === 'true' ? 'on' : 'off') : defaultValue}
                    </span>
                    {!isDefault && (
                      <Button
                        dense
                        variant="ghost"
                        aria-label={`Reset ${spec.label} to its default`}
                        onClick={() => {
                          set(spec.key, defaultValue);
                        }}
                      >
                        use default
                      </Button>
                    )}
                  </span>
                </Td>
                <Td id={hintId} className="whitespace-normal text-muted">
                  {spec.hint}
                </Td>
              </tr>
            );
          })}
        </tbody>
      </Table>

      {saveError !== undefined && (
        <p role="alert" className="rounded-sm border border-danger bg-danger-subtle px-2 py-1 text-xs/5 text-danger">
          Could not save the settings: {errorText(saveError)}
        </p>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" variant="primary" disabled={invalid || dirty.length === 0 || saving}>
          {saving ? 'Saving…' : 'Save'}
        </Button>
        <Button
          disabled={dirty.length === 0 || saving}
          onClick={() => {
            setForm(toForm(body.settings));
            setSaveError(undefined);
          }}
        >
          Discard changes
        </Button>
        <Button variant="ghost" disabled={saving} onClick={onReload}>
          Reload
        </Button>
        <span role="status" className="text-xs/5 text-muted">
          {dirty.length === 0
            ? 'No unsaved changes.'
            : invalid
              ? 'Fix the marked fields to save.'
              : `${dirty.length} unsaved ${dirty.length === 1 ? 'change' : 'changes'}. Every change is recorded in the audit log.`}
        </span>
      </div>
    </form>
  );
}

function FieldInput({
  id,
  hintId,
  spec,
  value,
  invalid,
  onChange,
}: {
  id: string;
  hintId: string;
  spec: FieldSpec;
  value: string;
  invalid: boolean;
  onChange: (value: string) => void;
}) {
  const common = { id, 'aria-describedby': hintId, 'aria-invalid': invalid || undefined };

  switch (spec.kind) {
    case 'policy':
      return (
        <select
          {...common}
          value={value}
          onChange={e => {
            onChange(e.target.value);
          }}
          className={cx(INPUT_CLASS, 'w-28')}
        >
          {POLICIES.map(p => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
      );
    case 'switch':
      return (
        <input
          {...common}
          type="checkbox"
          checked={value === 'true'}
          onChange={e => {
            onChange(e.target.checked ? 'true' : 'false');
          }}
          className="size-4 accent-accent"
        />
      );
    case 'percent':
    case 'count':
    case 'duration':
      return (
        <span className="inline-flex items-center gap-1">
          <input
            {...common}
            type="text"
            inputMode={spec.kind === 'duration' ? 'text' : 'decimal'}
            value={value}
            onChange={e => {
              onChange(e.target.value);
            }}
            className={cx(INPUT_CLASS, 'w-24 font-mono', invalid && 'border-danger')}
          />
          {spec.kind === 'percent' && <span className="text-muted">%</span>}
          {spec.key === 'maxFinalityLagEpochs' || spec.key === 'soakEpochs' ? (
            <span className="text-muted">epochs</span>
          ) : null}
        </span>
      );
  }
}
