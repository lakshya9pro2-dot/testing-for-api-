create table if not exists public.watch_progress (
    id uuid primary key default gen_random_uuid(),
    user_id text not null,
    video_id integer not null,
    media_type text not null,
    total_time_ms bigint not null default 0,
    playback_time_ms bigint not null default 0,
    season_number integer not null default 0,
    episode_number integer not null default 0,
    device_host text,
    watched_id text not null,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    constraint watch_progress_media_type_check check (media_type in ('tv', 'movies')),
    constraint watch_progress_time_nonnegative_check check (total_time_ms >= 0 and playback_time_ms >= 0),
    constraint watch_progress_season_nonnegative_check check (season_number >= 0),
    constraint watch_progress_episode_nonnegative_check check (episode_number >= 0),
    constraint watch_progress_unique_user_watched unique (user_id, watched_id),
    constraint watch_progress_watched_id_format_check check (
        (media_type = 'movies' and watched_id = 'm' || video_id::text)
        or
        (media_type = 'tv' and watched_id = 'tv' || video_id::text)
    )
);

create index if not exists watch_progress_user_id_idx on public.watch_progress (user_id);
create index if not exists watch_progress_updated_at_idx on public.watch_progress (updated_at desc);

create or replace function public.set_watch_progress_updated_at()
returns trigger
language plpgsql
set search_path = public
as $$
begin
    new.updated_at = now();
    return new;
end;
$$;

drop trigger if exists trg_watch_progress_updated_at on public.watch_progress;
create trigger trg_watch_progress_updated_at
before update on public.watch_progress
for each row execute function public.set_watch_progress_updated_at();

alter table public.watch_progress enable row level security;
