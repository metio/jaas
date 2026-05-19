local greeter = import 'greeter/main.libsonnet';

{
  formal: greeter.formal("Alice"),
  casual: greeter.casual("Bob"),
}
