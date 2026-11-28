import { useId, useMemo, useState, type SyntheticEvent } from 'react';
import { createPortal } from 'react-dom';

import { errorText } from '../api/client';
import { list, type Policy, type SettingsBody } from '../api/types';
import { useApp } from '../appContext';
import { useActions } from '../components/ActionDialog';
import { useLive } from '../components/Live';
import { RelTime } from '../components/RelTime';
import { IntentAction } from '../components/SelectionBar';
import { usePermissions } from '../components/Permissions';
import { Badge, Button, Card, ErrorBanner, INPUT_CLASS, InfoTip, Loading, Switch, cx } from '../components/primitives';
import { useQuery } from '../hooks/useQuery';
import { useUnsavedWork } from '../hooks/useUnsavedWork';
import { auditHash } from '../lib/auditCatalog';
import { absoluteTime } from '../lib/format';
import { armIntent, scopeIntent } from '../lib/intents';
import {
  changedSettings,
  dirtyKeys,
  displayValue,
  errorField,
  fieldSpec,
  toForm,
  validateForm,
  valueText,
  type FieldSpec,
  type SettingKey,
  type SettingsForm,
} from '../lib/settingsForm';
import { AREAS, type SectionSpec } from '../lib/settingsLayout';
import { ActionPolicies } from './ActionPolicies';
import { ImagePolicyMatrix } from './ImagePolicyMatrix';
import { FieldLabel, FieldRow, Mark, PolicySwitch, Problem, type FieldState } from './settingsParts';

/**
 * SettingsTab edits the scheduler settings, laid out by settingsLayout. The
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
    <div className="mx-auto grid w-full max-w-6xl grid-cols-1 gap-8 lg:grid-cols-[11rem_minmax(0,1fr)]">
      <SectionNav />
      <div className="flex min-w-0 flex-col gap-6">
        <StatusCards />
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
      </div>
    </div>
  );
}

const sectionId = (section: SectionSpec) => `settings-${section.id}`;

/** SectionNav lists the sections beside the form on a wide window; a click scrolls to one. */
function SectionNav() {
  return (
    <nav aria-label="Settings sections" className="hidden lg:block">
      <div className="sticky top-20 flex flex-col gap-4">
        {AREAS.map(area => (
          <div key={area.id} className="flex flex-col gap-1">
            <span className="px-2 text-xs/5 font-semibold tracking-wide text-faint uppercase">{area.title}</span>
            {area.sections.map(section => (
              <button
                key={section.id}
                type="button"
                onClick={() => {
                  document.getElementById(sectionId(section))?.scrollIntoView({ behavior: 'smooth', block: 'start' });
                }}
                className="rounded-sm px-2 py-1 text-left text-sm/5 text-muted hover:bg-surface-hover hover:text-foreground"
              >
                {section.title}
              </button>
            ))}
          </div>
        ))}
      </div>
    </nav>
  );
}

/** StatusCards state what the scheduler does now, apart from what can be edited. */
function StatusCards() {
  const plan = useLive('plan').data;
  const actions = useActions();
  if (!plan) return null;

  const w = plan.deployWindow;

  return (
    <div className="flex flex-col gap-4">
      {actions.notice}
      <Card
        title="Initial deployment window"
        aside={w.open && <IntentAction icon="check" label="Arm now…" onAction={actions.request} intent={armIntent()} />}
      >
        <p aria-label="Deployment window state" className="text-sm/6">
          {w.armedAt !== undefined ? (
            <>
              Armed <RelTime value={w.armedAt} />.
            </>
          ) : w.until === undefined ? (
            'Disabled.'
          ) : w.open ? (
            <>
              <Badge tone="warning">Open</Badge> until {absoluteTime(w.until)}, <RelTime value={w.until} />.
            </>
          ) : (
            <>
              Ended <RelTime value={w.until} />.
            </>
          )}
        </p>
        <p className="text-xs/5 text-muted">
          After the first start on a fresh store only Ansible deploys are granted until it ends. Its length is the
          controller&rsquo;s <span className="font-mono">deployWindow</span> config key.
        </p>
      </Card>
      {actions.dialog}
    </div>
  );
}

/** Field is what a section needs to render its settings. */
type Field = (key: SettingKey) => FieldState;

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
  const { can, audit } = usePermissions();
  const mayWrite = can({ action: 'settings' });

  const [form, setForm] = useState<SettingsForm>(() => toForm(body.settings));
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<{ error: unknown; field: SettingKey | undefined } | undefined>(undefined);

  const errors = useMemo(() => validateForm(form), [form]);
  const dirty = useMemo(() => dirtyKeys(form, body.settings), [form, body.settings]);
  const invalid = Object.keys(errors).length > 0;
  useUnsavedWork(dirty.length > 0);

  const field: Field = key => ({
    value: form[key],
    stored: displayValue(body.settings, key),
    fallback: displayValue(body.defaults, key),
    error: errors[key] ?? (saveError?.field === key ? errorText(saveError.error) : undefined),
    dirty: dirty.includes(key),
    disabled: saving || !mayWrite.allowed,
    reason: mayWrite.allowed ? undefined : mayWrite.reason,
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
    <form id={formId} aria-label="Settings" onSubmit={e => void submit(e)} className="flex flex-col gap-8" noValidate>
      {AREAS.map(area => (
        <div key={area.id} className="flex flex-col gap-4">
          <h2 className="text-xs/5 font-semibold tracking-wide text-faint uppercase">{area.title}</h2>
          {area.sections.map(section => (
            <SectionCard
              key={section.id}
              section={section}
              field={field}
              form={form}
              formId={formId}
              kinds={list(body.kinds)}
            />
          ))}
        </div>
      ))}

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
          {audit && (
            <a href={auditHash({ group: 'settings' })} className="ml-auto text-xs/5 text-accent hover:underline">
              Change history
            </a>
          )}
        </div>
      </div>
    </form>
  );
}

/**
 * SectionCard is one section: its title, what it governs and its master
 * control in the header, its settings in the form grid below. A setting
 * whose parent is another field of the section is drawn inside that field.
 */
function SectionCard({
  section,
  field,
  form,
  formId,
  kinds,
}: {
  section: SectionSpec;
  field: Field;
  form: SettingsForm;
  formId: string;
  /** kinds are the kinds of the fleet's managed containers. */
  kinds: readonly string[];
}) {
  const nested = new Set(section.fields.map(f => f.key));
  const rows = section.fields.filter(f => f.parent === undefined || !nested.has(f.parent));
  const master = section.master;
  const masterState = master && field(master.key);

  if (master?.byAction) return <PauseSection section={section} spec={master} />;

  // A field that inherits the master shows the master above its matrix, not in the header.
  const matrix = master && section.fields.find(f => f.kind === 'kinds' && f.parent === master.key);

  return (
    <section
      id={sectionId(section)}
      aria-label={section.title}
      className="scroll-mt-20 rounded-lg border border-border bg-surface"
    >
      <header className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3 border-b border-border px-4 py-4 sm:px-5">
        <div className="flex min-w-0 flex-col gap-0.5">
          <h3 className="text-sm/6 font-semibold">{section.title}</h3>
          <p className="text-xs/5 text-muted">{section.about}</p>
        </div>
        {master && masterState && !matrix && (
          <div className="flex flex-col items-start gap-1 sm:items-end">
            <PolicySwitch
              label={master.label}
              value={masterState.value as Policy}
              disabled={masterState.disabled}
              reason={masterState.reason}
              onChange={choice => {
                masterState.set(choice);
              }}
            />
            <Mark spec={master} state={masterState} />
            <Problem state={masterState} />
          </div>
        )}
      </header>
      <div className="flex flex-col divide-y divide-border px-4 sm:px-5">
        {rows.map(spec => {
          const id = `${formId}-${spec.key}`;
          switch (spec.kind) {
            case 'kinds': {
              const child = section.fields.find(f => f.parent === spec.key);
              if (!master || !masterState) return null;
              return (
                <ImagePolicyMatrix
                  key={spec.key}
                  root={{ spec: master, state: masterState }}
                  kinds={{ spec, state: field(spec.key) }}
                  teams={child && { spec: child, state: field(child.key) }}
                  form={form}
                  fleetKinds={kinds}
                />
              );
            }
            case 'actions':
              return <ActionPolicies key={spec.key} spec={spec} state={field(spec.key)} />;
            default:
              return <SettingRow key={spec.key} spec={spec} state={field(spec.key)} id={id} />;
          }
        })}
      </div>
      {section.clientPolicyNote && (
        <p className="border-t border-border px-4 py-3 text-xs/5 text-muted sm:px-5">
          A team can also set its own policy on the{' '}
          <a href="#clients" className="text-accent underline">
            Clients tab
          </a>
          .
        </p>
      )}
    </section>
  );
}

/**
 * PauseSection is the global pause: the switch above every rollout. It is
 * the pause and resume action with its confirmation, not a field of the form,
 * and shows the state the scheduler last planned with.
 */
function PauseSection({ section, spec }: { section: SectionSpec; spec: FieldSpec }) {
  const plan = useLive('plan').data;
  const actions = useActions();
  const paused = plan?.paused ?? false;

  return (
    <section
      id={sectionId(section)}
      aria-label={section.title}
      className={cx(
        'scroll-mt-20 rounded-lg border',
        paused ? 'border-warning bg-warning-subtle' : 'border-border-strong bg-surface'
      )}
    >
      {actions.notice}
      <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-3 px-4 py-4 sm:px-5">
        <div className="flex min-w-0 flex-col gap-0.5">
          <div className="flex items-center gap-1.5">
            <h3 className="text-sm/6 font-semibold">{section.title}</h3>
            <InfoTip label={spec.label}>
              <span className="block">{spec.hint}</span>
              <span className="mt-1 block text-muted">
                Setting: <span className="font-mono">{spec.key}</span>
              </span>
            </InfoTip>
          </div>
          <p className="text-xs/5 text-muted">{section.about}</p>
        </div>
        <div className="flex items-center gap-3">
          <span aria-label="Global pause state">
            {paused ? <Badge tone="warning">Paused</Badge> : <Badge tone="success">Running</Badge>}
          </span>
          {plan &&
            (paused ? (
              <IntentAction
                icon="resume"
                label="Resume all…"
                onAction={actions.request}
                intent={scopeIntent('resume', 'Resume everything', '')}
              />
            ) : (
              <IntentAction
                icon="pause"
                label="Pause all…"
                onAction={actions.request}
                intent={scopeIntent('pause', 'Pause everything', '')}
              />
            ))}
        </div>
      </div>
      {/* Outside the form: Enter in the dialog must not submit the settings. */}
      {actions.dialog && createPortal(actions.dialog, document.body)}
    </section>
  );
}

/** SettingRow is one setting in the form grid. */
function SettingRow({ spec, state, id }: { spec: FieldSpec; state: FieldState; id: string }) {
  const helpId = `${id}-help`;
  const labelled = spec.kind !== 'switch' && spec.kind !== 'policy';

  return (
    <FieldRow label={<FieldLabel spec={spec} state={state} htmlFor={labelled ? id : undefined} helpId={helpId} />}>
      <div className="flex min-h-7 items-center gap-2">
        <Control id={id} helpId={helpId} spec={spec} state={state} />
      </div>
      <Mark spec={spec} state={state} />
      <Problem state={state} />
    </FieldRow>
  );
}

/** Control is the input of a setting, with its unit. Inputs have one width, units one column. */
function Control({ id, helpId, spec, state }: { id: string; helpId: string; spec: FieldSpec; state: FieldState }) {
  const invalid = state.error !== undefined;

  switch (spec.kind) {
    case 'actions':
    case 'kinds':
    case 'teams':
      // The policies per name have editors of their own.
      return null;
    case 'switch':
      return (
        <>
          <Switch
            label={spec.label}
            describedBy={helpId}
            checked={state.value === 'true'}
            disabled={state.disabled}
            onChange={on => {
              state.set(on ? 'true' : 'false');
            }}
          />
          <span className="text-xs/5 text-muted">{state.value === 'true' ? 'On' : 'Off'}</span>
        </>
      );
    case 'policy':
      return (
        <PolicySwitch
          label={spec.label}
          value={state.value as Policy}
          disabled={state.disabled}
          reason={state.reason}
          onChange={choice => {
            state.set(choice);
          }}
        />
      );
    case 'duration':
      return (
        <input
          id={id}
          aria-describedby={helpId}
          aria-invalid={invalid || undefined}
          type="text"
          placeholder="e.g. 30m"
          value={state.value}
          disabled={state.disabled}
          onChange={e => {
            state.set(e.target.value);
          }}
          className={cx(INPUT_CLASS, 'w-28 text-right font-mono max-sm:h-10', invalid && 'border-danger')}
        />
      );
    case 'percent':
    case 'share':
    case 'count':
      return (
        <>
          <input
            id={id}
            aria-describedby={helpId}
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
            className={cx(INPUT_CLASS, 'w-28 text-right font-mono max-sm:h-10', invalid && 'border-danger')}
          />
          {spec.unit && <span className="text-xs/5 text-muted">{spec.unit}</span>}
        </>
      );
  }
}
