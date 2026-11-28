import { list, type Policy } from '../api/types';
import { useLive } from '../components/Live';
import { cx } from '../components/primitives';
import {
  POLICY_LABEL,
  kindPolicy,
  kindRows,
  parsePolicies,
  parseTeamPolicies,
  policiesText,
  teamPoliciesText,
  teamRows,
  type FieldSpec,
  type SettingsForm,
} from '../lib/settingsForm';
import { PolicySwitch, TreeMarks, TreeRow, type FieldState, type SwitchChoice } from './settingsParts';

/** MatrixField is a setting of the matrix with its form state. */
interface MatrixField {
  spec: FieldSpec;
  state: FieldState;
}

/** withEntry returns the policies with one entry set to its own policy, or removed to inherit. */
function withEntry(policies: Record<string, Policy>, name: string, choice: SwitchChoice): Record<string, Policy> {
  const next = { ...policies };
  if (choice === 'inherit') Reflect.deleteProperty(next, name);
  else next[name] = choice;
  return next;
}

/**
 * ImagePolicyMatrix edits the image policies: the policy for images
 * (policyImage) on top, then a table with one column per container kind, its
 * kind defaults (policyKinds) as the first row and one row per team
 * (policyClients). A kind inherits the images policy, a team's cell the
 * policy of its column's kind, unless it has its own.
 */
export function ImagePolicyMatrix({
  root,
  kinds,
  teams,
  form,
  fleetKinds,
}: {
  root: MatrixField;
  kinds: MatrixField;
  teams: MatrixField | undefined;
  form: SettingsForm;
  /** fleetKinds are the kinds of the fleet's managed containers. */
  fleetKinds: readonly string[];
}) {
  const clients = list(useLive('clients').data?.clients);
  const kindPolicies = parsePolicies(kinds.state.value) ?? {};
  const teamPolicies = parseTeamPolicies(teams?.state.value ?? '{}') ?? {};
  const byKind = teamRows(
    clients.map(c => ({ name: c.name, kinds: list(c.images).map(i => i.kind) })),
    parseTeamPolicies(teams?.state.stored ?? '{}') ?? {},
    teamPolicies
  );
  const allKinds = kindRows([...fleetKinds, ...byKind.keys()], parsePolicies(kinds.state.stored) ?? {}, kindPolicies);
  // A kind is a column when a team runs it or has a policy for it; the others are rows below.
  const columns = teams ? allKinds.filter(k => (byKind.get(k)?.length ?? 0) > 0) : [];
  const alone = allKinds.filter(k => !columns.includes(k));
  const teamNames = [...new Set(columns.flatMap(k => byKind.get(k) ?? []))].sort();
  const kindCell = (kind: string) => (
    <PolicyCell
      label={`Image policy of the kind ${kind}`}
      own={kindPolicies[kind]}
      inherited={imagePolicy}
      chain={`inherits all images: ${imagePolicy}`}
      state={kinds.state}
      onChange={choice => {
        kinds.state.set(policiesText(withEntry(kindPolicies, kind, choice)));
      }}
    />
  );
  const imagePolicy = root.state.value as Policy;

  return (
    <div className="flex flex-col gap-3 py-4">
      <p className="text-xs/5 text-muted">
        A kind inherits the policy for images, a team the policy of the kind in its column, unless it has its own.
        Inherited values are outlined dashed. Kinds no team runs are listed below the table.
      </p>
      <TreeRow depth={0} name={<span className="text-sm/6 font-medium">All images</span>}>
        <PolicySwitch
          label={root.spec.label}
          value={imagePolicy}
          disabled={root.state.disabled}
          reason={root.state.reason}
          onChange={choice => {
            if (choice !== 'inherit') root.state.set(choice);
          }}
        />
      </TreeRow>
      {columns.length > 0 && (
        <div className="max-w-full overflow-x-auto rounded-md border border-border">
          <table
            aria-label="Image policies per kind and team"
            className="w-full border-separate border-spacing-0 text-xs/5"
          >
            <thead>
              <tr>
                <th
                  scope="col"
                  className={cx(STICKY, 'border-b border-border px-3 py-2 text-left font-medium text-muted')}
                >
                  Team
                </th>
                {columns.map(kind => (
                  <th
                    key={kind}
                    scope="col"
                    className="border-b border-border px-2 py-2 text-left font-mono font-medium whitespace-nowrap"
                  >
                    {kind}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              <tr aria-label="Kind defaults" className="bg-surface-raised">
                <th
                  scope="row"
                  className={cx(STICKY, 'bg-surface-raised px-3 py-2 text-left font-medium whitespace-nowrap')}
                >
                  Kind default
                </th>
                {columns.map(kind => (
                  <td key={kind} className="px-2 py-2">
                    {kindCell(kind)}
                  </td>
                ))}
              </tr>
              {teamNames.map(team => (
                <tr key={team} aria-label={`Team ${team}`}>
                  <th
                    scope="row"
                    className={cx(STICKY, 'border-t border-border px-3 py-1.5 text-left font-mono font-normal')}
                  >
                    {team}
                  </th>
                  {columns.map(kind => {
                    const runs = byKind.get(kind)?.includes(team) ?? false;
                    const inherited = kindPolicy(form, kind);
                    return (
                      <td key={kind} className="border-t border-border px-2 py-1.5">
                        {runs && teams ? (
                          <PolicyCell
                            label={`Image policy of the team ${team} for ${kind}`}
                            own={teamPolicies[kind]?.[team]}
                            inherited={inherited}
                            chain={`inherits ${kind}: ${inherited}`}
                            state={teams.state}
                            onChange={choice => {
                              teams.state.set(
                                teamPoliciesText({
                                  ...teamPolicies,
                                  [kind]: withEntry(teamPolicies[kind] ?? {}, team, choice),
                                })
                              );
                            }}
                          />
                        ) : (
                          <span aria-label={`${team} runs no ${kind}`} className="px-2 text-faint">
                            –
                          </span>
                        )}
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {alone.length > 0 && (
        <ul aria-label="Kinds without teams" className="flex flex-col">
          {alone.map(kind => (
            <li key={kind} aria-label={`Kind ${kind}`}>
              <TreeRow
                depth={0}
                name={
                  <span className="font-mono text-xs/5 font-medium">
                    {kind === 'other' ? 'other (unlabelled)' : kind}
                  </span>
                }
              >
                {kindCell(kind)}
              </TreeRow>
            </li>
          ))}
        </ul>
      )}
      <TreeMarks fields={[root, kinds, ...(teams ? [teams] : [])]} />
    </div>
  );
}

/** STICKY keeps the first column in view while the table scrolls sideways. */
const STICKY = 'sticky left-0 z-10 bg-surface';

/**
 * PolicyCell is one entry of the matrix as a compact select: inherit (named
 * with the value it follows, outlined dashed), auto or manual (solid).
 */
function PolicyCell({
  label,
  own,
  inherited,
  chain,
  state,
  onChange,
}: {
  label: string;
  own: Policy | undefined;
  inherited: Policy;
  /** chain says what an inheriting cell follows, for its tooltip. */
  chain: string;
  state: FieldState;
  onChange: (choice: SwitchChoice) => void;
}) {
  const inherits = own === undefined;

  return (
    <select
      aria-label={label}
      value={own ?? 'inherit'}
      disabled={state.disabled}
      title={state.disabled && state.reason ? state.reason : inherits ? chain : `own policy: ${own}`}
      onChange={e => {
        onChange(e.target.value as SwitchChoice);
      }}
      className={cx(
        'h-7 w-36 cursor-pointer rounded-sm border px-1.5 text-xs/5 font-medium disabled:cursor-not-allowed disabled:opacity-60 max-sm:h-9',
        inherits
          ? 'border-dashed border-accent bg-surface text-muted'
          : 'border-border-strong bg-surface-selected text-foreground'
      )}
    >
      <option value="inherit">Inherit · {inherited}</option>
      <option value="auto">{POLICY_LABEL.auto}</option>
      <option value="manual">{POLICY_LABEL.manual}</option>
    </select>
  );
}
