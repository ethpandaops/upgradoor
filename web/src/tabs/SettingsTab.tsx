import { useId, useMemo, useState, type SyntheticEvent } from 'react';

import { errorText } from '../api/client';
import { list, type Policy, type SettingsBody } from '../api/types';
import { useApp } from '../appContext';
import { useLive } from '../components/Live';
import { usePermissions } from '../components/Permissions';
import {
  Badge,
  Button,
  Card,
  ErrorBanner,
  INPUT_CLASS,
  InfoTip,
  Loading,
  Segmented,
  Switch,
  cx,
} from '../components/primitives';
import { useQuery } from '../hooks/useQuery';
import { ActionPolicies } from './ActionPolicies';
import { DataSourcesCard } from './DataSourcesCard';
import {
  GROUPS,
  POLICIES,
  POLICY_MEANING,
  changedSettings,
  dirtyKeys,
  displayValue,
  errorField,
  fieldSpec,
  sameValue,
  shortValue,
  toForm,
  validateForm,
  valueText,
  type FieldSpec,
  type GroupSpec,
  type RolloutKindSpec,
  type SettingKey,
  type SettingsForm,
} from '../lib/settingsForm';

/**
 * SettingsTab edits the scheduler settings, grouped by what they govern. The
 * form is loaded once (it is not refreshed by the stream, so typing is never
 * overwritten). Saving sends the settings that changed, and nothing else.
 */
export function SettingsTab() {
  const { api } = useApp();
  const loaded = useQuery(s => api.settings(s), { key: 'settings-form' });
  const [saved, setSaved] = useState<SettingsBody | null>(null);

  // The answer of the last save is newer than what was loaded.
  const body = saved ?? loaded.data;

  return (
    <div className="mx-auto flex w-full max-w-screen-2xl flex-col gap-4">
      {body ? (
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
      ) : loaded.loading ? (
        <Loading what="settings" />
      ) : (
        <ErrorBanner error={loaded.error} what="the settings" onRetry={loaded.refresh} />
      )}
      <DataSourcesCard />
    </div>
  );
}

/** What every control of the form needs to know about its setting. */
interface FieldState {
  value: string;
  /** stored and fallback are the saved value and the default, as their inputs would show them. */
  stored: string;
  fallback: string;
  /** error is what is wrong with the input, or what the controller said about it. */
  error: string | undefined;
  dirty: boolean;
  disabled: boolean;
  set: (value: string) => void;
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
  const mayWrite = usePermissions().can({ action: 'settings' });

  const [form, setForm] = useState<SettingsForm>(() => toForm(body.settings));
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<{ error: unknown; field: SettingKey | undefined } | undefined>(undefined);

  const errors = useMemo(() => validateForm(form), [form]);
  const dirty = useMemo(() => dirtyKeys(form, body.settings), [form, body.settings]);
  const invalid = Object.keys(errors).length > 0;

  const field = (key: SettingKey): FieldState => ({
    value: form[key],
    stored: displayValue(body.settings, key),
    fallback: displayValue(body.defaults, key),
    error: errors[key] ?? (saveError?.field === key ? errorText(saveError.error) : undefined),
    dirty: dirty.includes(key),
    disabled: saving || !mayWrite.allowed,
    set: value => {
      setForm(prev => ({ ...prev, [key]: value }));
      // What the controller said was about the value that was sent.
      if (saveError?.field === key) setSaveError(undefined);
    },
  });

  const submit = async (e: SyntheticEvent) => {
    e.preventDefault();
    if (invalid || dirty.length === 0 || saving || !mayWrite.allowed) return;

    setSaving(true);
    setSaveError(undefined);
    try {
      // Only what changed: the controller keeps what the body does not name.
      onSaved(await api.putSettings(changedSettings(form, body.settings)));
    } catch (error) {
      setSaveError({ error, field: errorField(errorText(error)) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <form id={formId} aria-label="Settings" onSubmit={e => void submit(e)} className="flex flex-col gap-4" noValidate>
      {/*
        The scheduler's switches as a strip over the whole width, and below it
        three groups side by side, stretched to one height; on a narrow window
        everything is one column.
      */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        {GROUPS.map(group => (
          <GroupCard key={group.id} group={group} field={field} formId={formId} />
        ))}
      </div>

      <div className="sticky bottom-0 flex flex-col gap-2 rounded-lg border border-border bg-surface p-3 shadow-md">
        {/* What a save will send, before it is sent. */}
        {dirty.length > 0 && (
          <ul aria-label="Changes to save" className="flex flex-wrap gap-x-6 gap-y-1 text-xs/5">
            {dirty.map(key => {
              const spec = fieldSpec(key);
              return (
                <li key={key} className={cx(errors[key] && 'text-danger')}>
                  <span className="font-medium">{spec.label}</span>{' '}
                  <span className="font-mono text-muted">{valueText(spec, displayValue(body.settings, key))}</span>
                  {' → '}
                  <span className="font-mono">{valueText(spec, form[key])}</span>
                </li>
              );
            })}
          </ul>
        )}

        {saveError !== undefined && (
          <p role="alert" className="rounded-sm border border-danger bg-danger-subtle px-2 py-1 text-xs/5 text-danger">
            Could not save the settings: {errorText(saveError.error)}
          </p>
        )}

        <div className="flex flex-wrap items-center gap-2">
          <Button
            type="submit"
            variant="primary"
            disabled={invalid || dirty.length === 0 || saving || !mayWrite.allowed}
            title={mayWrite.allowed ? undefined : mayWrite.reason}
          >
            {saving ? 'Saving…' : dirty.length > 1 ? `Save ${dirty.length} changes` : 'Save'}
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
            {!mayWrite.allowed
              ? `Read only: ${mayWrite.reason}.`
              : dirty.length === 0
                ? 'No unsaved changes.'
                : invalid
                  ? 'Fix the marked fields to save.'
                  : `${dirty.length} unsaved ${dirty.length === 1 ? 'change' : 'changes'}. Every change is recorded in the audit log.`}
          </span>
        </div>
      </div>
    </form>
  );
}

function GroupCard({
  group,
  field,
  formId,
}: {
  group: GroupSpec;
  field: (key: SettingKey) => FieldState;
  formId: string;
}) {
  if (group.wide) {
    return (
      <Card
        title={group.title}
        className="lg:col-span-3"
        aside={<span className="text-xs/5 text-muted">{group.about}</span>}
      >
        <div className="grid grid-cols-1 gap-x-6 gap-y-4 sm:grid-cols-2 xl:grid-cols-5">
          {group.keys.map(key => (
            <SettingCell key={key} spec={fieldSpec(key)} state={field(key)} id={`${formId}-${key}`} />
          ))}
          {group.kinds?.map(kind => (
            <KindCell
              key={kind.id}
              kind={kind}
              policy={field(kind.policy)}
              canary={field(kind.canary)}
              formId={formId}
            />
          ))}
          {group.actionPolicies && <ActionPolicies state={field('actionPolicies')} />}
        </div>
        {group.kinds && <ClientPolicies />}
      </Card>
    );
  }

  return (
    <Card title={group.title} className="h-full">
      <p className="text-xs/5 text-muted">{group.about}</p>
      <div className="flex flex-col">
        {group.keys.map(key => (
          <SettingRow key={key} spec={fieldSpec(key)} state={field(key)} id={`${formId}-${key}`} />
        ))}
      </div>
    </Card>
  );
}

/**
 * ClientPolicies names the clients whose image policy is their own
 * (policyClients). It is read-only here and current: it is read from the
 * event stream, and changed on the Clients tab, client by client.
 */
function ClientPolicies() {
  const own = list(useLive('clients').data?.clients).filter(c => c.policySet);

  return (
    <p aria-label="Image policy per client" className="text-xs/5 text-muted">
      Image policy per client:{' '}
      {own.length === 0 ? (
        'every client follows the policy for images.'
      ) : (
        <span className="text-foreground">{own.map(c => `${c.name} ${c.policy}`).join(', ')}.</span>
      )}{' '}
      It is set on the{' '}
      <a href="#clients" className="text-accent underline">
        Clients tab
      </a>
      .
    </p>
  );
}

/** InfoText is what stands behind a setting's info icon: what it does, its valid range, its default. */
function InfoText({ spec, fallback }: { spec: FieldSpec; fallback: string }) {
  return (
    <>
      <span className="block">{spec.hint}</span>
      {spec.range && <span className="mt-1 block text-muted">Valid: {spec.range}.</span>}
      <span className="mt-1 block text-muted">
        Default: {valueText(spec, fallback)}. Setting: <span className="font-mono">{spec.key}</span>
      </span>
    </>
  );
}

/**
 * Mark stands in the line of a setting, between its label and its control,
 * so it takes no room when there is nothing to say and moves nothing when
 * there is: that the value is changed and not saved, and that it differs from
 * the default, with the default. The default is a button that puts it back.
 */
function Mark({ spec, state, what }: { spec: FieldSpec; state: FieldState; what: string }) {
  const isDefault = sameValue(spec, state.value, state.fallback);
  if (!state.dirty && isDefault) return null;

  return (
    <span className="inline-flex items-center gap-1.5 text-xs/5 whitespace-nowrap">
      {state.dirty && <Badge tone="info">changed</Badge>}
      {!isDefault && (
        <button
          type="button"
          disabled={state.disabled}
          aria-label={`Reset ${what} to its default`}
          title="This is not the default. Click to put the default back."
          onClick={() => {
            state.set(state.fallback);
          }}
          className="rounded-xs px-1 text-warning hover:bg-surface-hover disabled:cursor-not-allowed"
        >
          default {shortValue(spec, state.fallback)}
        </button>
      )}
    </span>
  );
}

/** Problem is what is wrong with an input, under its line. It is there only while something is wrong. */
function Problem({ state }: { state: FieldState }) {
  if (!state.error) return null;

  return (
    <span role="alert" className="text-right text-xs/5 text-danger">
      {state.error}
    </span>
  );
}

const ROW = 'flex flex-col gap-0.5 border-b border-border py-2.5 last:border-b-0 last:pb-0';

/** SettingRow is one setting in one line: its label and info, the mark, its control. */
function SettingRow({ spec, state, id }: { spec: FieldSpec; state: FieldState; id: string }) {
  const hintId = `${id}-hint`;

  return (
    <div className={ROW}>
      <div className="flex min-h-7 flex-wrap items-center gap-x-2 gap-y-1">
        <label htmlFor={id} className="text-sm/6 font-medium whitespace-nowrap">
          {spec.label}
        </label>
        <InfoTip label={spec.label}>
          <InfoText spec={spec} fallback={state.fallback} />
        </InfoTip>
        <span id={hintId} className="sr-only">
          {spec.hint} {spec.range ? `Valid: ${spec.range}.` : ''}
        </span>
        <span className="ml-auto inline-flex items-center gap-1.5">
          <Mark spec={spec} state={state} what={spec.label} />
          <Control id={id} hintId={hintId} spec={spec} state={state} />
        </span>
      </div>
      <Problem state={state} />
    </div>
  );
}

const CELL = 'flex flex-col gap-2 rounded-sm border border-border bg-background p-3';

/** SettingCell is one setting of a strip: its label and info above, its control below. */
function SettingCell({ spec, state, id }: { spec: FieldSpec; state: FieldState; id: string }) {
  const hintId = `${id}-hint`;

  return (
    <div className={CELL}>
      <div className="flex min-h-6 items-center gap-2">
        <label htmlFor={id} className="text-sm/6 font-medium whitespace-nowrap">
          {spec.label}
        </label>
        <InfoTip label={spec.label}>
          <InfoText spec={spec} fallback={state.fallback} />
        </InfoTip>
        <span id={hintId} className="sr-only">
          {spec.hint}
        </span>
        <span className="ml-auto">
          <Mark spec={spec} state={state} what={spec.label} />
        </span>
      </div>
      <div className="flex min-h-6 items-center gap-2 text-xs/5 text-muted">
        <Control id={id} hintId={hintId} spec={spec} state={state} />
        {spec.kind === 'switch' && (state.value === 'true' ? 'on: nothing new is granted' : 'off: the scheduler runs')}
      </div>
      <Problem state={state} />
    </div>
  );
}

/**
 * KindCell is one kind of rollout: its policy as a choice of three, and under
 * it the switch that pauses a rollout of the kind after its canary.
 */
function KindCell({
  kind,
  policy,
  canary,
  formId,
}: {
  kind: RolloutKindSpec;
  policy: FieldState;
  canary: FieldState;
  formId: string;
}) {
  const policySpec = fieldSpec(kind.policy);
  const canarySpec = fieldSpec(kind.canary);
  const canaryHint = `${formId}-${kind.canary}-hint`;

  return (
    <div className={CELL}>
      <div className="flex min-h-6 items-center gap-2">
        <span className="text-sm/6 font-medium whitespace-nowrap">{kind.label}</span>
        <InfoTip label={policySpec.label}>
          <span className="block">
            What the scheduler does when hosts report {kind.what}:
            {POLICIES.map(p => (
              <span key={p} className="block">
                <span className="font-mono">{p}</span>: {POLICY_MEANING[p]}
              </span>
            ))}
          </span>
          <span className="mt-1 block text-muted">
            Default: {policy.fallback}. Setting: <span className="font-mono">{kind.policy}</span>
          </span>
        </InfoTip>
        <span className="ml-auto inline-flex items-center gap-1.5">
          <Mark spec={policySpec} state={policy} what={policySpec.label} />
          <Segmented
            label={policySpec.label}
            value={policy.value as Policy}
            disabled={policy.disabled}
            onChange={policy.set}
            options={POLICIES.map(p => ({ value: p, label: p, title: POLICY_MEANING[p] }))}
          />
        </span>
      </div>
      <div className="flex min-h-6 items-center gap-2 text-xs/5 text-muted">
        <span>Pause after the canary</span>
        <InfoTip label={canarySpec.label}>
          <InfoText spec={canarySpec} fallback={canary.fallback} />
        </InfoTip>
        <span id={canaryHint} className="sr-only">
          {canarySpec.hint}
        </span>
        <span className="ml-auto inline-flex items-center gap-1.5">
          <Mark spec={canarySpec} state={canary} what={canarySpec.label} />
          <Switch
            label={canarySpec.label}
            describedBy={canaryHint}
            checked={canary.value === 'true'}
            disabled={canary.disabled}
            onChange={on => {
              canary.set(on ? 'true' : 'false');
            }}
          />
        </span>
      </div>
      <Problem state={policy} />
      <Problem state={canary} />
    </div>
  );
}

/** Control is the input of a setting, with its unit. Its width is fixed: a value that grows moves nothing. */
function Control({ id, hintId, spec, state }: { id: string; hintId: string; spec: FieldSpec; state: FieldState }) {
  const invalid = state.error !== undefined;

  switch (spec.kind) {
    case 'actions':
      // The policies per host action have an editor of their own: ActionPolicies.
      return null;
    case 'switch':
      return (
        <Switch
          label={spec.label}
          describedBy={hintId}
          checked={state.value === 'true'}
          disabled={state.disabled}
          onChange={on => {
            state.set(on ? 'true' : 'false');
          }}
        />
      );
    case 'policy':
      return (
        <Segmented
          label={spec.label}
          value={state.value as Policy}
          disabled={state.disabled}
          onChange={state.set}
          options={POLICIES.map(p => ({ value: p, label: p, title: POLICY_MEANING[p] }))}
        />
      );
    case 'duration':
      return (
        <>
          <input
            id={id}
            aria-describedby={hintId}
            aria-invalid={invalid || undefined}
            type="text"
            placeholder="e.g. 30m"
            value={state.value}
            disabled={state.disabled}
            onChange={e => {
              state.set(e.target.value);
            }}
            className={cx(INPUT_CLASS, 'w-24 text-right font-mono', invalid && 'border-danger')}
          />
          {/* The place of a unit, so the inputs of a card line up. */}
          <span className="w-16" />
        </>
      );
    case 'percent':
    case 'share':
    case 'count':
      return (
        <>
          <input
            id={id}
            aria-describedby={hintId}
            aria-invalid={invalid || undefined}
            type="number"
            inputMode="decimal"
            min={spec.min}
            max={spec.max}
            step={spec.step ?? 1}
            value={state.value}
            disabled={state.disabled}
            onChange={e => {
              state.set(e.target.value);
            }}
            className={cx(INPUT_CLASS, 'w-24 text-right font-mono', invalid && 'border-danger')}
          />
          {spec.unit && <span className="w-16 text-xs/5 text-muted">{spec.unit}</span>}
        </>
      );
  }
}
