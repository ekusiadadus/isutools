-- Read-only ISUCON14 adapter for cmd/isutools-trajectory.
-- Replace these three values with the saved isutools run boundary and ID.
SET @run_start = '2026-08-05 21:39:00.000000';
SET @run_end = '2026-08-05 21:42:00.000000';
SET @artifact_id = 'replace-with-artifact-id';

WITH
run_rides AS (
  SELECT r.*
  FROM rides AS r
  WHERE r.created_at BETWEEN @run_start AND @run_end
),
run_agents AS (
  SELECT DISTINCT chair_id AS id
  FROM run_rides
  WHERE chair_id IS NOT NULL
),
ranked_opening_points AS (
  SELECT cl.*,
         ROW_NUMBER() OVER (
           PARTITION BY cl.chair_id
           ORDER BY cl.created_at DESC, cl.id DESC
         ) AS row_number
  FROM chair_locations AS cl
  JOIN run_agents AS a ON a.id = cl.chair_id
  WHERE cl.created_at < @run_start
),
run_points AS (
  SELECT chair_id, created_at, latitude, longitude
  FROM ranked_opening_points
  WHERE row_number = 1
  UNION ALL
  SELECT cl.chair_id, cl.created_at, cl.latitude, cl.longitude
  FROM chair_locations AS cl
  JOIN run_agents AS a ON a.id = cl.chair_id
  WHERE cl.created_at BETWEEN @run_start AND @run_end
)
SELECT CAST(JSON_OBJECT(
  'type', 'meta',
  'schema', 1,
  'title', CONCAT('ISUCON14 ', @artifact_id)
) AS CHAR) AS record
UNION ALL
SELECT CAST(JSON_OBJECT(
  'type', 'agent',
  'id', c.id,
  'label', c.name,
  'kind', c.model
) AS CHAR)
FROM chairs AS c
JOIN run_agents AS a ON a.id = c.id
UNION ALL
SELECT CAST(JSON_OBJECT(
  'type', 'point',
  'agent_id', p.chair_id,
  'at', CONCAT(DATE_FORMAT(p.created_at, '%Y-%m-%dT%H:%i:%s.%f'), '+09:00'),
  'x', p.latitude,
  'y', p.longitude
) AS CHAR)
FROM run_points AS p
UNION ALL
SELECT CAST(JSON_OBJECT(
  'type', 'job',
  'id', r.id,
  'requested_at', CONCAT(DATE_FORMAT(r.created_at, '%Y-%m-%dT%H:%i:%s.%f'), '+09:00'),
  'pickup', JSON_OBJECT('x', r.pickup_latitude, 'y', r.pickup_longitude),
  'destination', JSON_OBJECT('x', r.destination_latitude, 'y', r.destination_longitude),
  'finished_at', (
    SELECT CONCAT(DATE_FORMAT(MIN(rs.created_at), '%Y-%m-%dT%H:%i:%s.%f'), '+09:00')
    FROM ride_statuses AS rs
    WHERE rs.ride_id = r.id AND rs.status = 'COMPLETED'
  )
) AS CHAR)
FROM run_rides AS r
UNION ALL
SELECT CAST(JSON_OBJECT(
  'type', 'assignment',
  'job_id', r.id,
  'agent_id', r.chair_id,
  'at', CONCAT(DATE_FORMAT((
    SELECT MIN(rs.created_at)
    FROM ride_statuses AS rs
    WHERE rs.ride_id = r.id AND rs.status = 'ENROUTE'
  ), '%Y-%m-%dT%H:%i:%s.%f'), '+09:00')
) AS CHAR)
FROM run_rides AS r
WHERE r.chair_id IS NOT NULL
  AND EXISTS (
    SELECT 1
    FROM ride_statuses AS rs
    WHERE rs.ride_id = r.id AND rs.status = 'ENROUTE'
  );
