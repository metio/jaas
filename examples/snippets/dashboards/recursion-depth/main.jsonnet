local recurse(n) = if n <= 0 then 0 else recurse(n - 1) + 1;

function(depth="10")
  local n = std.parseInt(depth);
  {
    requested_depth: n,
    actual_count: recurse(n),
  }
