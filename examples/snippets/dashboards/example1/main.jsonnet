local examplonet = import 'examplonet/main.libsonnet';

{
  person1: {
    name: std.extVar('name'),
    welcome: 'Hello ' + self.name + '!',
    external: std.extVar('key')
  },
  person2: self.person1 { name: 'Bob' },
} + examplonet
