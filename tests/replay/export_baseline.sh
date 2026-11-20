#!/usr/bin/env bash
# Exports the stored conversations of a long-evaluation database (the baseline's idp_host.dump, restored into a PostgreSQL of
# your own) to a directory the replay test reads: IDP_CONTEXT_REPLAY_DIR=<dir> go test -run TestReplay -v ./internal/harness
# The content is a third party's business data: keep the directory outside the repository.
#
#   tests/replay/export_baseline.sh <psql command prefix, e.g. "docker exec my-pg psql -U postgres -d b"> <out dir>
set -euo pipefail
PSQL=$1
OUT=$2
mkdir -p "$OUT"
q() { $PSQL -At -c "$1"; }
# the server's timings (D54) exist in dumps made after migration 0011; an older dump has none (null)
if [ "$(q "select count(*) from information_schema.columns where table_name='run_steps' and column_name='server_prompt_n'")" = 1 ]; then
  SPN=server_prompt_n SCN=server_cache_n FOM=first_output_ms
else
  SPN='step*0+null' SCN='step*0+null' FOM='step*0+null'
fi
for t in $(q "select distinct thread_id from runs where thread_id like 'le-%' order by 1"); do
  q "select json_build_object('thread','$t',
       'runs',(select coalesce(json_agg(json_build_object('id',r.id,'createdAt',r.created_at,
          'steps',(select coalesce(json_agg(json_build_object('step',s.step,'startedAt',s.started_at,'prompt',s.prompt_tokens,'completion',s.completion_tokens,'error',s.error_code,'serverPromptN',s.${SPN},'serverCacheN',s.${SCN},'firstOutputMs',s.${FOM}) order by s.step),'[]'::json) from run_steps s where s.run_id=r.id)) order by r.created_at),'[]'::json) from runs r where r.thread_id='$t'),
       'messages',(select coalesce(json_agg(json_build_object('seq',seq,'createdAt',created_at,'message',message) order by seq),'[]'::json) from thread_messages where thread_id='$t'))" > "$OUT/$t.json"
done
# the effective instructions of the release the evaluation ran (the agent's, then its skill's)
q "select (document::json)->'definition'->>'instructions' from agent_releases where document like '%office-assistant%'" > "$OUT/instructions.txt"
echo >> "$OUT/instructions.txt"
q "select (document::json)->'definition'->>'instructions' from skill_releases" >> "$OUT/instructions.txt"
