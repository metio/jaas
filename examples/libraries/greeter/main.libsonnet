local utils = import 'utils/main.libsonnet';

{
  formal(name): utils.greeting(name),
  casual(name): "hey " + std.asciiLower(name),
}
